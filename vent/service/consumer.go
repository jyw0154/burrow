package service

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/hyperledger/burrow/execution/evm/abi"
	"github.com/hyperledger/burrow/execution/exec"
	"github.com/hyperledger/burrow/rpc/rpcevents"
	"github.com/hyperledger/burrow/rpc/rpcquery"
	"github.com/hyperledger/burrow/vent/config"
	"github.com/hyperledger/burrow/vent/logger"
	"github.com/hyperledger/burrow/vent/sqldb"
	"github.com/hyperledger/burrow/vent/sqlsol"
	"github.com/hyperledger/burrow/vent/types"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
)

// Consumer contains basic configuration for consumer to run
type Consumer struct {
	Config         *config.Flags
	Log            *logger.Logger
	Closing        bool
	DB             *sqldb.SQLDB
	GRPCConnection *grpc.ClientConn
	// external events channel used for when vent is leveraged as a library
	EventsChannel chan types.EventData
}

// NewConsumer constructs a new consumer configuration.
// The event channel will be passed a collection of rows generated from all of the events in a single block
// It will be closed by the consumer when it is finished
func NewConsumer(cfg *config.Flags, log *logger.Logger, eventChannel chan types.EventData) *Consumer {
	return &Consumer{
		Config:        cfg,
		Log:           log,
		Closing:       false,
		EventsChannel: eventChannel,
	}
}

// Run connects to a grpc service and subscribes to log events,
// then gets tables structures, maps them & parse event data.
// Store data in SQL event tables, it runs forever
func (c *Consumer) Run(projection *sqlsol.Projection, abiSpec *abi.AbiSpec, stream bool) error {
	var err error

	c.Log.Info("msg", "Connecting to Burrow gRPC server")

	c.GRPCConnection, err = grpc.Dial(c.Config.GRPCAddr, grpc.WithInsecure())
	if err != nil {
		return errors.Wrapf(err, "Error connecting to Burrow gRPC server at %s", c.Config.GRPCAddr)
	}
	defer c.GRPCConnection.Close()

	// get the chain ID to compare with the one stored in the db
	qCli := rpcquery.NewQueryClient(c.GRPCConnection)
	chainStatus, err := qCli.Status(context.Background(), &rpcquery.StatusParam{})
	if err != nil {
		return errors.Wrapf(err, "Error getting chain status")
	}

	if len(projection.EventSpec) == 0 {
		c.Log.Info("msg", "No events specifications found")
		return nil
	}

	c.Log.Info("msg", "Connecting to SQL database")

	connection := types.SQLConnection{
		DBAdapter:     c.Config.DBAdapter,
		DBURL:         c.Config.DBURL,
		DBSchema:      c.Config.DBSchema,
		Log:           c.Log,
		ChainID:       chainStatus.ChainID,
		BurrowVersion: chainStatus.BurrowVersion,
	}

	c.DB, err = sqldb.NewSQLDB(connection)
	if err != nil {
		return fmt.Errorf("error connecting to SQL database: %v", err)
	}
	defer c.DB.Close()

	c.Log.Info("msg", "Synchronizing config and database projection structures")

	err = c.DB.SynchronizeDB(projection.Tables)
	if err != nil {
		return errors.Wrap(err, "Error trying to synchronize database")
	}

	// doneCh is used for sending a "done" signal from each goroutine to the main thread
	// eventCh is used for sending received events to the main thread to be stored in the db
	doneCh := make(chan error)
	eventCh := make(chan types.EventData)

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()

		c.Log.Info("msg", "Getting last processed block number from SQL log table")

		// NOTE [Silas]: I am preserving the comment below that dates from the early days of Vent. I have looked at the
		// bosmarmot git history and I cannot see why the original author thought that it was the case that there was
		// no way of knowing if the last block of events was committed since the block and its associated log is
		// committed atomically in a transaction and this is a core part of he design of Vent - in order that it does not
		// repeat

		// right now there is no way to know if the last block of events was completely read
		// so we have to begin processing from the last block number stored in database
		// and update event data if already present
		fromBlock, err := c.DB.GetLastBlockHeight()
		if err != nil {
			doneCh <- errors.Wrapf(err, "Error trying to get last processed block number from SQL log table")
			return
		}

		startingBlock := fromBlock
		// Start the block after the last one successfully committed - apart from if this is the first block
		// We include block 0 because it is where we currently place dump/restored transactions
		if startingBlock > 0 {
			startingBlock++
		}

		// setup block range to get needed blocks server side
		cli := rpcevents.NewExecutionEventsClient(c.GRPCConnection)
		var end *rpcevents.Bound
		if stream {
			end = rpcevents.StreamBound()
		} else {
			end = rpcevents.LatestBound()
		}

		request := &rpcevents.BlocksRequest{
			BlockRange: rpcevents.NewBlockRange(rpcevents.AbsoluteBound(startingBlock), end),
		}

		// gets blocks in given range based on last processed block taken from database
		stream, err := cli.Stream(context.Background(), request)
		if err != nil {
			doneCh <- errors.Wrapf(err, "Error connecting to block stream")
			return
		}

		// get blocks

		c.Log.Debug("msg", "Waiting for blocks...")

		err = rpcevents.ConsumeBlockExecutions(stream, func(blockExecution *exec.BlockExecution) error {
			if c.Closing {
				return io.EOF
			}
			c.Log.Debug("msg", "Block received", "height", blockExecution.Height, "num_txs", len(blockExecution.TxExecutions))

			// set new block number
			fromBlock = blockExecution.Height

			// create a fresh new structure to store block data at this height
			blockData := sqlsol.NewBlockData(fromBlock)

			if c.Config.DBBlockTx {
				blkRawData, err := buildBlkData(projection.Tables, blockExecution)
				if err != nil {
					doneCh <- errors.Wrapf(err, "Error building block raw data")
				}
				// set row in structure
				blockData.AddRow(types.SQLBlockTableName, blkRawData)
			}

			// get transactions for a given block
			for _, txe := range blockExecution.TxExecutions {
				c.Log.Debug("msg", "Getting transaction", "TxHash", txe.TxHash, "num_events", len(txe.Events))

				if c.Config.DBBlockTx {
					txRawData, err := buildTxData(projection.Tables, txe)
					if err != nil {
						doneCh <- errors.Wrapf(err, "Error building tx raw data")
					}
					// set row in structure
					blockData.AddRow(types.SQLTxTableName, txRawData)
				}

				// reverted transactions don't have to update event data tables
				// so check that condition to filter them
				if txe.Exception == nil {

					// get events for a given transaction
					for _, event := range txe.Events {

						taggedEvent := event.Tagged()

						// see which spec filter matches with the one in event data
						for _, eventClass := range projection.EventSpec {
							qry, err := eventClass.Query()

							if err != nil {
								doneCh <- errors.Wrapf(err, "Error parsing query from filter string")
								return io.EOF
							}

							// there's a matching filter, add data to the rows
							if qry.Matches(taggedEvent) {

								c.Log.Info("msg", fmt.Sprintf("Matched event header: %v", event.Header),
									"filter", eventClass.Filter)

								// unpack, decode & build event data
								eventData, err := buildEventData(projection, eventClass, event, abiSpec, c.Log)
								if err != nil {
									doneCh <- errors.Wrapf(err, "Error building event data")
								}

								// set row in structure
								blockData.AddRow(eventClass.TableName, eventData)
							}
						}
					}
				}
			}

			// upsert rows in specific SQL event tables and update block number
			// store block data in SQL tables (if any)
			if blockData.PendingRows(fromBlock) {
				// gets block data to upsert
				blk := blockData.Data

				c.Log.Info("msg", fmt.Sprintf("Upserting rows in SQL tables %v", blk), "block", fromBlock)

				eventCh <- blk
			}
			return nil
		})

		if err != nil {
			if err == io.EOF {
				c.Log.Debug("msg", "EOF stream received...")
			} else {
				if c.Closing {
					c.Log.Debug("msg", "GRPC connection closed")
				} else {
					doneCh <- errors.Wrapf(err, "Error receiving blocks")
					return
				}
			}
		}
	}()

	go func() {
		// wait for all threads to end
		wg.Wait()
		doneCh <- nil
	}()

loop:
	for {
		select {
		case err := <-doneCh:
			if err != nil {
				return err
			}
			break loop
		case blk := <-eventCh:
			// upsert rows in specific SQL event tables and update block number
			if err := c.DB.SetBlock(projection.Tables, blk); err != nil {
				return errors.Wrap(err, "Error upserting rows in SQL event tables")
			}

			// send to the external events channel in a non-blocking manner
			select {
			case c.EventsChannel <- blk:
			default:
			}
		}
	}

	close(c.EventsChannel)
	c.Log.Info("msg", "Done!")
	return nil
}

// Health returns the health status for the consumer
func (c *Consumer) Health() error {
	if c.Closing {
		return errors.New("closing service")
	}

	// check db status
	if c.DB == nil {
		return errors.New("database disconnected")
	}

	if err := c.DB.Ping(); err != nil {
		return errors.New("database unavailable")
	}

	// check grpc connection status
	if c.GRPCConnection == nil {
		return errors.New("grpc disconnected")
	}

	if grpcState := c.GRPCConnection.GetState(); grpcState != connectivity.Ready {
		return errors.New("grpc connection not ready")
	}

	return nil
}

// Shutdown gracefully shuts down the events consumer
func (c *Consumer) Shutdown() {
	c.Log.Info("msg", "Shutting down vent consumer...")
	c.Closing = true
	c.GRPCConnection.Close()
}
