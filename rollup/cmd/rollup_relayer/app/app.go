package app

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/scroll-tech/da-codec/encoding"
	"github.com/scroll-tech/go-ethereum/common"
	"github.com/scroll-tech/go-ethereum/ethclient"
	"github.com/scroll-tech/go-ethereum/log"
	"github.com/scroll-tech/go-ethereum/rollup/l1"
	"github.com/urfave/cli/v2"
	"gorm.io/gorm"

	"scroll-tech/common/database"
	"scroll-tech/common/observability"
	"scroll-tech/common/types"
	"scroll-tech/common/utils"
	"scroll-tech/common/version"
	"scroll-tech/rollup/internal/config"
	"scroll-tech/rollup/internal/controller/relayer"
	"scroll-tech/rollup/internal/controller/watcher"
	"scroll-tech/rollup/internal/orm"
	butils "scroll-tech/rollup/internal/utils"
)

var app *cli.App

func init() {
	// Set up rollup-relayer app info.
	app = cli.NewApp()
	app.Action = action
	app.Name = "rollup-relayer"
	app.Usage = "The Scroll Rollup Relayer"
	app.Version = version.Version
	app.Flags = append(app.Flags, utils.CommonFlags...)
	app.Flags = append(app.Flags, utils.RollupRelayerFlags...)
	app.Commands = []*cli.Command{}
	app.Before = func(ctx *cli.Context) error {
		return utils.LogSetup(ctx)
	}
	// Register `rollup-relayer-test` app for integration-test.
	utils.RegisterSimulation(app, utils.RollupRelayerApp)
}

func action(ctx *cli.Context) error {
	// Load config file.
	cfgFile := ctx.String(utils.ConfigFileFlag.Name)
	cfg, err := config.NewConfig(cfgFile)
	if err != nil {
		log.Crit("failed to load config file", "config file", cfgFile, "error", err)
	}

	subCtx, cancel := context.WithCancel(ctx.Context)
	// Init db connection
	db, err := database.InitDB(cfg.DBConfig)
	if err != nil {
		log.Crit("failed to init db connection", "err", err)
	}
	defer func() {
		cancel()
		if err = database.CloseDB(db); err != nil {
			log.Crit("failed to close db connection", "error", err)
		}
	}()

	registry := prometheus.DefaultRegisterer
	observability.Server(ctx, db)

	// Init l2geth connection
	l2client, err := ethclient.Dial(cfg.L2Config.Endpoint)
	if err != nil {
		log.Crit("failed to connect l2 geth", "config file", cfgFile, "error", err)
	}

	genesisPath := ctx.String(utils.Genesis.Name)
	genesis, err := utils.ReadGenesis(genesisPath)
	if err != nil {
		log.Crit("failed to read genesis", "genesis file", genesisPath, "error", err)
	}

	initGenesis := ctx.Bool(utils.ImportGenesisFlag.Name)
	l2relayer, err := relayer.NewLayer2Relayer(ctx.Context, l2client, db, cfg.L2Config.RelayerConfig, genesis.Config, initGenesis, relayer.ServiceTypeL2RollupRelayer, registry)
	if err != nil {
		log.Crit("failed to create l2 relayer", "config file", cfgFile, "error", err)
	}

	chunkProposer := watcher.NewChunkProposer(subCtx, cfg.L2Config.ChunkProposerConfig, genesis.Config, db, registry)
	batchProposer := watcher.NewBatchProposer(subCtx, cfg.L2Config.BatchProposerConfig, genesis.Config, db, registry)
	bundleProposer := watcher.NewBundleProposer(subCtx, cfg.L2Config.BundleProposerConfig, genesis.Config, db, registry)

	l2watcher := watcher.NewL2WatcherClient(subCtx, l2client, cfg.L2Config.Confirmations, cfg.L2Config.L2MessageQueueAddress, cfg.L2Config.WithdrawTrieRootSlot, genesis.Config, db, registry)

	if cfg.RecoveryConfig.Enable {
		log.Info("Starting rollup-relayer in recovery mode", "version", version.Version)

		if err = restoreFullPreviousState(cfg, db, chunkProposer, batchProposer, bundleProposer, l2watcher); err != nil {
			log.Crit("failed to restore full previous state", "error", err)
		}

		return nil
	}

	// Watcher loop to fetch missing blocks
	go utils.LoopWithContext(subCtx, 2*time.Second, func(ctx context.Context) {
		number, loopErr := butils.GetLatestConfirmedBlockNumber(ctx, l2client, cfg.L2Config.Confirmations)
		if loopErr != nil {
			log.Error("failed to get block number", "err", loopErr)
			return
		}
		// errors are logged in the try method as well
		_ = l2watcher.TryFetchRunningMissingBlocks(number)
	})

	go utils.Loop(subCtx, time.Duration(cfg.L2Config.ChunkProposerConfig.ProposeIntervalMilliseconds)*time.Millisecond, chunkProposer.TryProposeChunk)

	go utils.Loop(subCtx, time.Duration(cfg.L2Config.BatchProposerConfig.ProposeIntervalMilliseconds)*time.Millisecond, batchProposer.TryProposeBatch)

	go utils.Loop(subCtx, 10*time.Second, bundleProposer.TryProposeBundle)

	go utils.Loop(subCtx, 2*time.Second, l2relayer.ProcessPendingBatches)

	go utils.Loop(subCtx, 15*time.Second, l2relayer.ProcessCommittedBatches)

	go utils.Loop(subCtx, 15*time.Second, l2relayer.ProcessPendingBundles)

	// Finish start all rollup relayer functions.
	log.Info("Start rollup-relayer successfully", "version", version.Version)

	// Catch CTRL-C to ensure a graceful shutdown.
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	// Wait until the interrupt signal is received from an OS signal.
	<-interrupt

	return nil
}

// Run rollup relayer cmd instance.
func Run() {
	if err := app.Run(os.Args); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func restoreFullPreviousState(cfg *config.Config, db *gorm.DB, chunkProposer *watcher.ChunkProposer, batchProposer *watcher.BatchProposer, bundleProposer *watcher.BundleProposer, l2Watcher *watcher.L2WatcherClient) error {
	log.Info("Restoring full previous state with", "L1 block height", cfg.RecoveryConfig.L1BlockHeight, "latest finalized batch", cfg.RecoveryConfig.LatestFinalizedBatch)

	// DB state should be clean: the latest batch in the DB should be finalized on L1. This function will
	// restore all batches between the latest finalized batch in the DB and the latest finalized batch on L1.

	// 1. Get latest finalized batch stored in DB
	latestDBBatch, err := batchProposer.BatchORM().GetLatestBatch(context.Background())
	if err != nil {
		return fmt.Errorf("failed to get latest batch from DB: %w", err)
	}
	fmt.Println("latestDBBatch", latestDBBatch)

	// TODO:
	//  1. what if it is a fresh start? -> latest batch is nil
	//latestDBBatch.CommitTxHash

	log.Info("Latest finalized batch in DB", "batch", latestDBBatch.Index, "hash", latestDBBatch.Hash)

	// TODO: make these parameters -> part of genesis config?
	scrollChainAddress := common.HexToAddress("0x2D567EcE699Eabe5afCd141eDB7A4f2D0D6ce8a0")
	l1MessageQueueAddress := common.HexToAddress("0xF0B2293F5D834eAe920c6974D50957A1732de763")

	l1Client, err := ethclient.Dial(cfg.L1Config.Endpoint)
	if err != nil {
		return fmt.Errorf("failed to connect to L1 client: %w", err)
	}
	reader, err := l1.NewReader(context.Background(), l1.Config{
		ScrollChainAddress:    scrollChainAddress,
		L1MessageQueueAddress: l1MessageQueueAddress,
	}, l1Client)
	if err != nil {
		return fmt.Errorf("failed to create L1 reader: %w", err)
	}

	// 2. Get latest finalized L1 block
	latestFinalizedL1Block, err := reader.GetLatestFinalizedBlockNumber()
	if err != nil {
		return fmt.Errorf("failed to get latest finalized L1 block number: %w", err)
	}

	log.Info("Latest finalized L1 block number", "latest finalized L1 block", latestFinalizedL1Block)

	// 3. Get latest finalized batch from contract (at latest finalized L1 block)
	latestFinalizedBatch, err := reader.LatestFinalizedBatch(latestFinalizedL1Block)
	if err != nil {
		return fmt.Errorf("failed to get latest finalized batch: %w", err)
	}

	log.Info("Latest finalized batch from L1 contract", "latest finalized batch", latestFinalizedBatch, "at latest finalized L1 block", latestFinalizedL1Block)

	// 4. Get batches one by one from stored in DB to latest finalized batch.
	receipt, err := l1Client.TransactionReceipt(context.Background(), common.HexToHash(latestDBBatch.CommitTxHash))
	if err != nil {
		return fmt.Errorf("failed to get transaction receipt of latest DB batch finalization transaction: %w", err)
	}
	fromBlock := receipt.BlockNumber.Uint64()

	log.Info("Fetching rollup events from L1", "from block", fromBlock, "to block", latestFinalizedL1Block, "from batch", latestDBBatch.Index, "to batch", latestFinalizedBatch)

	commitsHeapMap := common.NewHeapMap[uint64, *l1.CommitBatchEvent](func(event *l1.CommitBatchEvent) uint64 {
		return event.BatchIndex().Uint64()
	})
	batchEventsHeap := common.NewHeap[*batchEvents]()
	var bundles [][]*batchEvents

	err = reader.FetchRollupEventsInRangeWithCallback(fromBlock, latestFinalizedL1Block, func(event l1.RollupEvent) bool {
		// We're only interested in batches that are newer than the latest finalized batch in the DB.
		if event.BatchIndex().Uint64() <= latestDBBatch.Index {
			return true
		}

		switch event.Type() {
		case l1.CommitEventType:
			commitEvent := event.(*l1.CommitBatchEvent)
			commitsHeapMap.Push(commitEvent)

		case l1.FinalizeEventType:
			finalizeEvent := event.(*l1.FinalizeBatchEvent)

			var bundle []*batchEvents

			// with bundles all commited batches until this finalized batch are finalized in the same bundle
			for commitsHeapMap.Len() > 0 {
				commitEvent := commitsHeapMap.Peek()
				if commitEvent.BatchIndex().Uint64() > finalizeEvent.BatchIndex().Uint64() {
					break
				}

				bEvents := newBatchEvents(commitEvent, finalizeEvent)
				commitsHeapMap.Pop()
				batchEventsHeap.Push(bEvents)
				bundle = append(bundle, bEvents)
			}

			bundles = append(bundles, bundle)

			// Stop fetching rollup events if we reached the latest finalized batch.
			if finalizeEvent.BatchIndex().Uint64() >= latestFinalizedBatch {
				return false
			}

		case l1.RevertEventType:
			// We ignore reverted batches.
			commitsHeapMap.RemoveByKey(event.BatchIndex().Uint64())
		}

		return true
	})
	if err != nil {
		return fmt.Errorf("failed to fetch rollup events: %w", err)
	}

	// 5. Process all finalized batches: fetch L2 blocks and reproduce chunks and batches.
	for batchEventsHeap.Len() > 0 {
		nextBatch := batchEventsHeap.Pop().Value()
		fmt.Println("nextBatch", nextBatch.commit.BatchIndex(), nextBatch.commit.BatchHash(), nextBatch.finalize.BatchIndex(), nextBatch.finalize.BatchHash())
		if err = processFinalizedBatch(db, reader, nextBatch, chunkProposer, batchProposer, l2Watcher); err != nil {
			return fmt.Errorf("failed to process finalized batch %d %s: %w", nextBatch.commit.BatchIndex(), nextBatch.commit.BatchHash(), err)
		}

		log.Info("Processed finalized batch", "batch", nextBatch.commit.BatchIndex(), "hash", nextBatch.commit.BatchHash())
	}

	// 6. Create bundles if needed.
	for _, bundle := range bundles {
		var dbBatches []*orm.Batch
		var lastBatchInBundle *orm.Batch

		for _, batch := range bundle {
			dbBatch, err := batchProposer.BatchORM().GetBatchByIndex(context.Background(), batch.commit.BatchIndex().Uint64())
			if err != nil {
				return fmt.Errorf("failed to get batch by index for bundle generation: %w", err)
			}
			// Bundles are only supported for codec version 3 and above.
			if encoding.CodecVersion(dbBatch.CodecVersion) < encoding.CodecV3 {
				break
			}

			dbBatches = append(dbBatches, dbBatch)
			lastBatchInBundle = dbBatch
		}

		if len(dbBatches) == 0 {
			continue
		}

		err = db.Transaction(func(dbTX *gorm.DB) error {
			newBundle, err := bundleProposer.BundleORM().InsertBundle(context.Background(), dbBatches, encoding.CodecVersion(lastBatchInBundle.CodecVersion), dbTX)
			if err != nil {
				return fmt.Errorf("failed to insert bundle to DB: %w", err)
			}
			if err = batchProposer.BatchORM().UpdateBundleHashInRange(context.Background(), newBundle.StartBatchIndex, newBundle.EndBatchIndex, newBundle.Hash, dbTX); err != nil {
				return fmt.Errorf("failed to update bundle_hash %s for batches (%d to %d): %w", newBundle.Hash, newBundle.StartBatchIndex, newBundle.EndBatchIndex, err)
			}

			if err = bundleProposer.BundleORM().UpdateFinalizeTxHashAndRollupStatus(context.Background(), newBundle.Hash, lastBatchInBundle.FinalizeTxHash, types.RollupFinalized, dbTX); err != nil {
				return fmt.Errorf("failed to update finalize tx hash and rollup status for bundle %s: %w", newBundle.Hash, err)
			}

			if err = bundleProposer.BundleORM().UpdateProvingStatus(context.Background(), newBundle.Hash, types.ProvingTaskVerified, dbTX); err != nil {
				return fmt.Errorf("failed to update proving status for bundle %s: %w", newBundle.Hash, err)
			}

			return nil
		})
		if err != nil {
			return fmt.Errorf("failed to insert bundle in DB transaction: %w", err)
		}

		fmt.Println("bundle", len(bundle), bundle[0].commit.BatchIndex())
	}

	return nil
}

func processFinalizedBatch(db *gorm.DB, reader *l1.Reader, nextBatch *batchEvents, chunkProposer *watcher.ChunkProposer, batchProposer *watcher.BatchProposer, l2Watcher *watcher.L2WatcherClient) error {
	log.Info("Processing finalized batch", "batch", nextBatch.commit.BatchIndex(), "hash", nextBatch.commit.BatchHash())

	// 5.1. Fetch commit tx data for batch (via commit event).
	args, err := reader.FetchCommitTxData(nextBatch.commit)
	if err != nil {
		return fmt.Errorf("failed to fetch commit tx data: %w", err)
	}

	codec, err := encoding.CodecFromVersion(encoding.CodecVersion(args.Version))
	if err != nil {
		return fmt.Errorf("failed to get codec: %w", err)
	}

	daChunksRawTxs, err := codec.DecodeDAChunksRawTx(args.Chunks)
	if err != nil {
		return fmt.Errorf("failed to decode DA chunks: %w", err)
	}
	lastChunk := daChunksRawTxs[len(daChunksRawTxs)-1]
	lastBlockInBatch := lastChunk.Blocks[len(lastChunk.Blocks)-1].Number()

	log.Info("Fetching L2 blocks from l2geth", "batch", nextBatch.commit.BatchIndex(), "last L2 block in batch", lastBlockInBatch)

	// 5.2. Fetch L2 blocks for the entire batch.
	if err = l2Watcher.TryFetchRunningMissingBlocks(lastBlockInBatch); err != nil {
		return fmt.Errorf("failed to fetch L2 blocks: %w", err)
	}

	// 5.3. Reproduce chunks.
	daChunks := make([]*encoding.Chunk, 0, len(daChunksRawTxs))
	dbChunks := make([]*orm.Chunk, 0, len(daChunksRawTxs))
	for _, daChunkRawTxs := range daChunksRawTxs {
		start := daChunkRawTxs.Blocks[0].Number()
		end := daChunkRawTxs.Blocks[len(daChunkRawTxs.Blocks)-1].Number()

		blocks, err := l2Watcher.BlockORM().GetL2BlocksInRange(context.Background(), start, end)
		if err != nil {
			return fmt.Errorf("failed to get L2 blocks in range: %w", err)
		}

		log.Info("Reproducing chunk", "start block", start, "end block", end)

		var chunk encoding.Chunk
		for _, block := range blocks {
			chunk.Blocks = append(chunk.Blocks, block)
		}

		metrics, err := butils.CalculateChunkMetrics(&chunk, codec.Version())
		if err != nil {
			return fmt.Errorf("failed to calculate chunk metrics: %w", err)
		}

		err = db.Transaction(func(dbTX *gorm.DB) error {
			dbChunk, err := chunkProposer.ChunkORM().InsertChunk(context.Background(), &chunk, codec.Version(), *metrics, dbTX)
			if err != nil {
				return fmt.Errorf("failed to insert chunk to DB: %w", err)
			}
			if err := l2Watcher.BlockORM().UpdateChunkHashInRange(context.Background(), dbChunk.StartBlockNumber, dbChunk.EndBlockNumber, dbChunk.Hash, dbTX); err != nil {
				return fmt.Errorf("failed to update chunk_hash for l2_blocks (chunk hash: %s, start block: %d, end block: %d): %w", dbChunk.Hash, dbChunk.StartBlockNumber, dbChunk.EndBlockNumber, err)
			}

			if err = chunkProposer.ChunkORM().UpdateProvingStatus(context.Background(), dbChunk.Hash, types.ProvingTaskVerified, dbTX); err != nil {
				return fmt.Errorf("failed to update proving status for chunk %s: %w", dbChunk.Hash, err)
			}

			daChunks = append(daChunks, &chunk)
			dbChunks = append(dbChunks, dbChunk)

			return nil
		})
		if err != nil {
			return fmt.Errorf("failed to insert chunk in DB transaction: %w", err)
		}
	}

	// 5.4 Reproduce batch.
	dbParentBatch, err := batchProposer.BatchORM().GetLatestBatch(context.Background())
	if err != nil {
		return fmt.Errorf("failed to get latest batch from DB: %w", err)
	}

	var batch encoding.Batch
	batch.Index = dbParentBatch.Index + 1
	batch.ParentBatchHash = common.HexToHash(dbParentBatch.Hash)
	batch.TotalL1MessagePoppedBefore = dbChunks[0].TotalL1MessagesPoppedBefore

	for _, chunk := range daChunks {
		batch.Chunks = append(batch.Chunks, chunk)
	}

	metrics, err := butils.CalculateBatchMetrics(&batch, codec.Version())
	if err != nil {
		return fmt.Errorf("failed to calculate batch metrics: %w", err)
	}

	err = db.Transaction(func(dbTX *gorm.DB) error {
		dbBatch, err := batchProposer.BatchORM().InsertBatch(context.Background(), &batch, codec.Version(), *metrics, dbTX)
		if err != nil {
			return fmt.Errorf("failed to insert batch to DB: %w", err)
		}
		if err = chunkProposer.ChunkORM().UpdateBatchHashInRange(context.Background(), dbBatch.StartChunkIndex, dbBatch.EndChunkIndex, dbBatch.Hash, dbTX); err != nil {
			return fmt.Errorf("failed to update batch_hash for chunks (batch hash: %s, start chunk: %d, end chunk: %d): %w", dbBatch.Hash, dbBatch.StartChunkIndex, dbBatch.EndChunkIndex, err)
		}

		if err = batchProposer.BatchORM().UpdateProvingStatus(context.Background(), dbBatch.Hash, types.ProvingTaskVerified, dbTX); err != nil {
			return fmt.Errorf("failed to update proving status for batch %s: %w", dbBatch.Hash, err)
		}
		if err = batchProposer.BatchORM().UpdateRollupStatusCommitAndFinalizeTxHash(context.Background(), dbBatch.Hash, types.RollupFinalized, nextBatch.commit.TxHash().Hex(), nextBatch.finalize.TxHash().Hex(), dbTX); err != nil {
			return fmt.Errorf("failed to update rollup status for batch %s: %w", dbBatch.Hash, err)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to insert batch in DB transaction: %w", err)
	}

	return nil
}

type batchEvents struct {
	commit   *l1.CommitBatchEvent
	finalize *l1.FinalizeBatchEvent
}

func newBatchEvents(commit *l1.CommitBatchEvent, finalize *l1.FinalizeBatchEvent) *batchEvents {
	if commit.BatchIndex().Uint64() != finalize.BatchIndex().Uint64() {
		panic("commit and finalize batch indexes do not match")
	}

	return &batchEvents{
		commit:   commit,
		finalize: finalize,
	}
}

func (e *batchEvents) CompareTo(other *batchEvents) int {
	return e.commit.BatchIndex().Cmp(other.commit.BatchIndex())
}
