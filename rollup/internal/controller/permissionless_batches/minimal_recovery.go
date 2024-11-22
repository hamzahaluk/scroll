package permissionless_batches

import (
	"context"
	"fmt"

	"github.com/scroll-tech/da-codec/encoding"
	"github.com/scroll-tech/go-ethereum/core"
	"github.com/scroll-tech/go-ethereum/ethclient"
	"github.com/scroll-tech/go-ethereum/log"
	"github.com/scroll-tech/go-ethereum/rollup/l1"
	"gorm.io/gorm"

	"scroll-tech/common/types"
	"scroll-tech/database/migrate"
	"scroll-tech/rollup/internal/config"
	"scroll-tech/rollup/internal/controller/watcher"
	"scroll-tech/rollup/internal/orm"
)

const (
	// defaultRestoredChunkIndex is the default index of the last restored fake chunk. It is used to be able to generate new chunks pretending that we have already processed some chunks.
	defaultRestoredChunkIndex uint64 = 1337
	// defaultRestoredBundleIndex is the default index of the last restored fake bundle. It is used to be able to generate new bundles pretending that we have already processed some bundles.
	defaultRestoredBundleIndex uint64 = 1
)

type MinimalRecovery struct {
	ctx       context.Context
	cfg       *config.Config
	genesis   *core.Genesis
	db        *gorm.DB
	chunkORM  *orm.Chunk
	batchORM  *orm.Batch
	bundleORM *orm.Bundle

	chunkProposer  *watcher.ChunkProposer
	batchProposer  *watcher.BatchProposer
	bundleProposer *watcher.BundleProposer
	l2Watcher      *watcher.L2WatcherClient
}

func NewRecovery(ctx context.Context, cfg *config.Config, genesis *core.Genesis, db *gorm.DB, chunkProposer *watcher.ChunkProposer, batchProposer *watcher.BatchProposer, bundleProposer *watcher.BundleProposer, l2Watcher *watcher.L2WatcherClient) *MinimalRecovery {
	return &MinimalRecovery{
		ctx:            ctx,
		cfg:            cfg,
		genesis:        genesis,
		db:             db,
		chunkORM:       orm.NewChunk(db),
		batchORM:       orm.NewBatch(db),
		bundleORM:      orm.NewBundle(db),
		chunkProposer:  chunkProposer,
		batchProposer:  batchProposer,
		bundleProposer: bundleProposer,
		l2Watcher:      l2Watcher,
	}
}

func (r *MinimalRecovery) RecoveryNeeded() bool {
	chunk, err := r.chunkORM.GetLatestChunk(r.ctx)
	if err != nil {
		return true
	}
	if chunk.Index <= defaultRestoredChunkIndex {
		return true
	}

	batch, err := r.batchORM.GetLatestBatch(r.ctx)
	if err != nil {
		return true
	}
	if batch.Index <= r.cfg.RecoveryConfig.LatestFinalizedBatch {
		return true
	}

	bundle, err := r.bundleORM.GetLatestBundle(r.ctx)
	if err != nil {
		return true
	}
	if bundle.Index <= defaultRestoredBundleIndex {
		return true
	}

	return false
}

func (r *MinimalRecovery) Run() error {
	// Make sure we start from a clean state.
	if err := r.resetDB(); err != nil {
		return fmt.Errorf("failed to reset DB: %w", err)
	}

	// Restore minimal previous state required to be able to create new chunks, batches and bundles.
	restoredFinalizedChunk, restoredFinalizedBatch, restoredFinalizedBundle, err := r.restoreMinimalPreviousState()
	if err != nil {
		return fmt.Errorf("failed to restore minimal previous state: %w", err)
	}

	// Fetch and insert the missing blocks from the last block in the latestFinalizedBatch to the latest L2 block.
	fromBlock := restoredFinalizedChunk.EndBlockNumber + 1
	toBlock, err := r.fetchL2Blocks(fromBlock, r.cfg.RecoveryConfig.L2BlockHeightLimit)
	if err != nil {
		return fmt.Errorf("failed to fetch L2 blocks: %w", err)
	}

	// Create chunks for L2 blocks.
	log.Info("Creating chunks for L2 blocks", "from", fromBlock, "to", toBlock)

	var latestChunk *orm.Chunk
	var count int
	for {
		if err = r.chunkProposer.ProposeChunk(); err != nil {
			return fmt.Errorf("failed to propose chunk: %w", err)
		}
		count++

		latestChunk, err = r.chunkORM.GetLatestChunk(r.ctx)
		if err != nil {
			return fmt.Errorf("failed to get latest latestFinalizedChunk: %w", err)
		}

		log.Info("Chunk created", "index", latestChunk.Index, "hash", latestChunk.Hash, "StartBlockNumber", latestChunk.StartBlockNumber, "EndBlockNumber", latestChunk.EndBlockNumber, "TotalL1MessagesPoppedBefore", latestChunk.TotalL1MessagesPoppedBefore)

		// We have created chunks for all available L2 blocks.
		if latestChunk.EndBlockNumber >= toBlock {
			break
		}
	}

	log.Info("Chunks created", "count", count, "latest latestFinalizedChunk", latestChunk.Index, "hash", latestChunk.Hash, "StartBlockNumber", latestChunk.StartBlockNumber, "EndBlockNumber", latestChunk.EndBlockNumber, "TotalL1MessagesPoppedBefore", latestChunk.TotalL1MessagesPoppedBefore)

	// Create batch for the created chunks. We only allow 1 batch it needs to be submitted (and finalized) with a proof in a single step.
	log.Info("Creating batch for chunks", "from", restoredFinalizedChunk.Index+1, "to", latestChunk.Index)

	r.batchProposer.TryProposeBatch()
	latestBatch, err := r.batchORM.GetLatestBatch(r.ctx)
	if err != nil {
		return fmt.Errorf("failed to get latest latestFinalizedBatch: %w", err)
	}

	// Sanity check that the batch was created correctly:
	// 1. should be a new batch
	// 2. should contain all chunks created
	if restoredFinalizedBatch.Index+1 != latestBatch.Index {
		return fmt.Errorf("batch was not created correctly, expected %d but got %d", restoredFinalizedBatch.Index+1, latestBatch.Index)
	}

	firstChunkInBatch, err := r.chunkORM.GetChunkByIndex(r.ctx, latestBatch.EndChunkIndex)
	if err != nil {
		return fmt.Errorf("failed to get first chunk in batch: %w", err)
	}
	lastChunkInBatch, err := r.chunkORM.GetChunkByIndex(r.ctx, latestBatch.EndChunkIndex)
	if err != nil {
		return fmt.Errorf("failed to get last chunk in batch: %w", err)
	}

	// Make sure that the batch contains all previously created chunks and thus all blocks. If not the user will need to
	// produce another batch (running the application again) starting from the end block of the last chunk in the batch + 1.
	if latestBatch.EndChunkIndex != latestChunk.Index {
		log.Warn("Produced batch does not contain all chunks and blocks. You'll need to produce another batch starting from end block+1.", "starting block", firstChunkInBatch.StartBlockNumber, "end block", lastChunkInBatch.EndBlockNumber, "latest block", latestChunk.EndBlockNumber)
	}

	log.Info("Batch created", "index", latestBatch.Index, "hash", latestBatch.Hash, "StartChunkIndex", latestBatch.StartChunkIndex, "EndChunkIndex", latestBatch.EndChunkIndex, "starting block", firstChunkInBatch.StartBlockNumber, "ending block", lastChunkInBatch.EndBlockNumber)

	r.bundleProposer.TryProposeBundle()
	latestBundle, err := r.bundleORM.GetLatestBundle(r.ctx)
	if err != nil {
		return fmt.Errorf("failed to get latest bundle: %w", err)
	}

	// Sanity check that the bundle was created correctly:
	// 1. should be a new bundle
	// 2. should only contain 1 batch, the one we created
	if restoredFinalizedBundle.Index == latestBundle.Index {
		return fmt.Errorf("bundle was not created correctly")
	}
	if latestBundle.StartBatchIndex != latestBatch.Index || latestBundle.EndBatchIndex != latestBatch.Index {
		return fmt.Errorf("bundle does not contain the correct batch: %d != %d", latestBundle.StartBatchIndex, latestBatch.Index)
	}

	log.Info("Bundle created", "index", latestBundle.Index, "hash", latestBundle.Hash, "StartBatchIndex", latestBundle.StartBatchIndex, "EndBatchIndex", latestBundle.EndBatchIndex, "starting block", firstChunkInBatch.StartBlockNumber, "ending block", lastChunkInBatch.EndBlockNumber)

	return nil
}

// restoreMinimalPreviousState restores the minimal previous state required to be able to create new chunks, batches and bundles.
func (r *MinimalRecovery) restoreMinimalPreviousState() (*orm.Chunk, *orm.Batch, *orm.Bundle, error) {
	log.Info("Restoring previous state with", "L1 block height", r.cfg.RecoveryConfig.L1BlockHeight, "latest finalized batch", r.cfg.RecoveryConfig.LatestFinalizedBatch)

	l1Client, err := ethclient.Dial(r.cfg.L1Config.Endpoint)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to connect to L1 client: %w", err)
	}
	reader, err := l1.NewReader(r.ctx, l1.Config{
		ScrollChainAddress:    r.genesis.Config.Scroll.L1Config.ScrollChainAddress,
		L1MessageQueueAddress: r.genesis.Config.Scroll.L1Config.L1MessageQueueAddress,
	}, l1Client)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create L1 reader: %w", err)
	}

	// 1. Sanity check user input: Make sure that the user's L1 block height is not higher than the latest finalized block number.
	latestFinalizedL1Block, err := reader.GetLatestFinalizedBlockNumber()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to get latest finalized L1 block number: %w", err)
	}
	if r.cfg.RecoveryConfig.L1BlockHeight > latestFinalizedL1Block {
		return nil, nil, nil, fmt.Errorf("specified L1 block height is higher than the latest finalized block number: %d > %d", r.cfg.RecoveryConfig.L1BlockHeight, latestFinalizedL1Block)
	}

	log.Info("Latest finalized L1 block number", "latest finalized L1 block", latestFinalizedL1Block)

	// 2. Make sure that the specified batch is indeed finalized on the L1 rollup contract and is the latest finalized batch.
	var latestFinalizedBatch uint64
	if r.cfg.RecoveryConfig.ForceLatestFinalizedBatch {
		latestFinalizedBatch = r.cfg.RecoveryConfig.LatestFinalizedBatch
	} else {
		latestFinalizedBatch, err = reader.LatestFinalizedBatch(latestFinalizedL1Block)
		if r.cfg.RecoveryConfig.LatestFinalizedBatch != latestFinalizedBatch {
			return nil, nil, nil, fmt.Errorf("batch %d is not the latest finalized batch: %d", r.cfg.RecoveryConfig.LatestFinalizedBatch, latestFinalizedBatch)
		}
	}

	// Find the commit event for the latest finalized batch.
	var batchCommitEvent *l1.CommitBatchEvent
	err = reader.FetchRollupEventsInRangeWithCallback(r.cfg.RecoveryConfig.L1BlockHeight, latestFinalizedL1Block, func(event l1.RollupEvent) bool {
		if event.Type() == l1.CommitEventType && event.BatchIndex().Uint64() == latestFinalizedBatch {
			batchCommitEvent = event.(*l1.CommitBatchEvent)
			// We found the commit event for the batch, stop searching.
			return false
		}

		// Continue until we find the commit event for the batch.
		return true
	})
	if batchCommitEvent == nil {
		return nil, nil, nil, fmt.Errorf("commit event not found for batch %d", latestFinalizedBatch)
	}

	log.Info("Found commit event for batch", "batch", batchCommitEvent.BatchIndex(), "hash", batchCommitEvent.BatchHash(), "L1 block height", batchCommitEvent.BlockNumber(), "L1 tx hash", batchCommitEvent.TxHash())

	// 3. Fetch commit tx data for latest finalized batch.
	args, err := reader.FetchCommitTxData(batchCommitEvent)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to fetch commit tx data: %w", err)
	}

	codec, err := encoding.CodecFromVersion(encoding.CodecVersion(args.Version))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to get codec: %w", err)
	}

	daChunksRawTxs, err := codec.DecodeDAChunksRawTx(args.Chunks)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to decode DA chunks: %w", err)
	}
	lastChunk := daChunksRawTxs[len(daChunksRawTxs)-1]
	lastBlockInBatch := lastChunk.Blocks[len(lastChunk.Blocks)-1].Number()

	log.Info("Last L2 block in batch", "batch", batchCommitEvent.BatchIndex(), "L2 block", lastBlockInBatch)

	// 4. Get the L1 messages count after the latest finalized batch.
	var l1MessagesCount uint64
	if r.cfg.RecoveryConfig.ForceL1MessageCount == 0 {
		l1MessagesCount, err = reader.FinalizedL1MessageQueueIndex(latestFinalizedL1Block)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to get L1 messages count: %w", err)
		}
	} else {
		l1MessagesCount = r.cfg.RecoveryConfig.ForceL1MessageCount
	}

	log.Info("L1 messages count after latest finalized batch", "batch", batchCommitEvent.BatchIndex(), "count", l1MessagesCount)

	// 5. Insert minimal state to DB.
	chunk, err := r.chunkORM.InsertChunkRaw(r.ctx, defaultRestoredChunkIndex, codec.Version(), lastChunk, l1MessagesCount)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to insert chunk raw: %w", err)
	}

	log.Info("Inserted last finalized chunk to DB", "chunk", chunk.Index, "hash", chunk.Hash, "StartBlockNumber", chunk.StartBlockNumber, "EndBlockNumber", chunk.EndBlockNumber, "TotalL1MessagesPoppedBefore", chunk.TotalL1MessagesPoppedBefore)

	batch, err := r.batchORM.InsertBatchRaw(r.ctx, batchCommitEvent.BatchIndex(), batchCommitEvent.BatchHash(), codec.Version(), chunk)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to insert batch raw: %w", err)
	}

	log.Info("Inserted last finalized batch to DB", "batch", batch.Index, "hash", batch.Hash)

	var bundle *orm.Bundle
	err = r.db.Transaction(func(dbTX *gorm.DB) error {
		bundle, err = r.bundleORM.InsertBundle(r.ctx, []*orm.Batch{batch}, encoding.CodecVersion(batch.CodecVersion), dbTX)
		if err != nil {
			return fmt.Errorf("failed to insert bundle: %w", err)
		}
		if err = r.bundleORM.UpdateProvingStatus(r.ctx, bundle.Hash, types.ProvingTaskVerified, dbTX); err != nil {
			return fmt.Errorf("failed to update proving status: %w", err)
		}
		if err = r.bundleORM.UpdateRollupStatus(r.ctx, bundle.Hash, types.RollupFinalized); err != nil {
			return fmt.Errorf("failed to update rollup status: %w", err)
		}

		log.Info("Inserted last finalized bundle to DB", "bundle", bundle.Index, "hash", bundle.Hash, "StartBatchIndex", bundle.StartBatchIndex, "EndBatchIndex", bundle.EndBatchIndex)

		return nil
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to insert bundle: %w", err)
	}

	return chunk, batch, bundle, nil
}

func (r *MinimalRecovery) fetchL2Blocks(fromBlock uint64, l2BlockHeightLimit uint64) (uint64, error) {
	if l2BlockHeightLimit > 0 && fromBlock > l2BlockHeightLimit {
		return 0, fmt.Errorf("fromBlock (latest finalized L2 block) is higher than specified L2BlockHeightLimit: %d > %d", fromBlock, l2BlockHeightLimit)
	}

	log.Info("Fetching L2 blocks with", "fromBlock", fromBlock, "l2BlockHeightLimit", l2BlockHeightLimit)

	// Fetch and insert the missing blocks from the last block in the batch to the latest L2 block.
	latestL2Block, err := r.l2Watcher.Client.BlockNumber(r.ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get latest L2 block number: %w", err)
	}

	log.Info("Latest L2 block number", "latest L2 block", latestL2Block)

	if l2BlockHeightLimit > latestL2Block {
		return 0, fmt.Errorf("l2BlockHeightLimit is higher than the latest L2 block number, not all blocks are available in L2geth: %d > %d", l2BlockHeightLimit, latestL2Block)
	}

	toBlock := latestL2Block
	if l2BlockHeightLimit > 0 {
		toBlock = l2BlockHeightLimit
	}

	err = r.l2Watcher.GetAndStoreBlocks(r.ctx, fromBlock, toBlock)
	if err != nil {
		return 0, fmt.Errorf("failed to get and store blocks: %w", err)
	}

	log.Info("Fetched L2 blocks from", "fromBlock", fromBlock, "toBlock", toBlock)

	return toBlock, nil
}

func (r *MinimalRecovery) resetDB() error {
	sqlDB, err := r.db.DB()
	if err != nil {
		return fmt.Errorf("failed to get db connection: %w", err)
	}

	// reset and init DB
	var v int64
	err = migrate.Rollback(sqlDB, &v)
	if err != nil {
		return fmt.Errorf("failed to rollback db: %w", err)
	}

	err = migrate.Migrate(sqlDB)
	if err != nil {
		return fmt.Errorf("failed to migrate db: %w", err)
	}

	return nil
}
