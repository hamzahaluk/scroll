package provertask

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/scroll-tech/da-codec/encoding"
	"github.com/scroll-tech/go-ethereum/common"
	"github.com/scroll-tech/go-ethereum/log"
	"github.com/scroll-tech/go-ethereum/params"
	"gorm.io/gorm"

	"scroll-tech/common/types"
	"scroll-tech/common/types/message"
	"scroll-tech/common/utils"

	"scroll-tech/coordinator/internal/config"
	"scroll-tech/coordinator/internal/orm"
	coordinatorType "scroll-tech/coordinator/internal/types"
)

// BatchProverTask handles batch proof tasks
type BatchProverTask struct {
	BaseProverTask

	batchTaskGetTaskTotal  *prometheus.CounterVec
	batchTaskGetTaskProver *prometheus.CounterVec
}

// NewBatchProverTask initializes a new BatchProverTask instance
func NewBatchProverTask(cfg *config.Config, chainCfg *params.ChainConfig, db *gorm.DB, reg prometheus.Registerer) *BatchProverTask {
	return &BatchProverTask{
		BaseProverTask: BaseProverTask{
			db:                 db,
			cfg:                cfg,
			chainCfg:           chainCfg,
			blockOrm:           orm.NewL2Block(db),
			chunkOrm:           orm.NewChunk(db),
			batchOrm:           orm.NewBatch(db),
			proverTaskOrm:      orm.NewProverTask(db),
			proverBlockListOrm: orm.NewProverBlockList(db),
		},
		batchTaskGetTaskTotal: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "coordinator_batch_get_task_total",
			Help: "Total number of batch get task.",
		}, []string{"fork_name"}),
		batchTaskGetTaskProver: newGetTaskCounterVec(promauto.With(reg), "batch"),
	}
}

// Assign assigns batch tasks to provers
func (bp *BatchProverTask) Assign(ctx *gin.Context, getTaskParameter *coordinatorType.GetTaskParameter) (*coordinatorType.GetTaskSchema, error) {
	taskCtx, err := bp.checkParameter(ctx)
	if err != nil || taskCtx == nil {
		return nil, fmt.Errorf("check prover task parameter failed, error: %w", err)
	}

	batchTask, err := bp.getBatchTask(ctx, getTaskParameter)
	if err != nil || batchTask == nil {
		return nil, err
	}

	hardForkName, err := bp.hardForkName(ctx, batchTask)
	if err != nil {
		bp.recoverActiveAttempts(ctx, batchTask)
		log.Error("retrieve hard fork name by batch failed", "task_id", batchTask.Hash, "err", err)
		return nil, ErrCoordinatorInternalFailure
	}

	proverTask := orm.ProverTask{
		TaskID:          batchTask.Hash,
		ProverPublicKey: taskCtx.PublicKey,
		TaskType:        int16(message.ProofTypeBatch),
		ProverName:      taskCtx.ProverName,
		ProverVersion:   taskCtx.ProverVersion,
		ProvingStatus:   int16(types.ProverAssigned),
		FailureType:     int16(types.ProverTaskFailureTypeUndefined),
		AssignedAt:      utils.NowUTC(),
	}

	if err = bp.proverTaskOrm.InsertProverTask(ctx.Copy(), &proverTask); err != nil {
		bp.recoverActiveAttempts(ctx, batchTask)
		log.Error("insert batch prover task info fail", "task_id", batchTask.Hash, "publicKey", taskCtx.PublicKey, "err", err)
		return nil, ErrCoordinatorInternalFailure
	}

	taskMsg, err := bp.formatProverTask(ctx.Copy(), &proverTask, batchTask, hardForkName)
	if err != nil {
		bp.recoverActiveAttempts(ctx, batchTask)
		log.Error("format prover task failure", "task_id", batchTask.Hash, "err", err)
		return nil, ErrCoordinatorInternalFailure
	}

	bp.recordMetrics(hardForkName, &proverTask)

	return taskMsg, nil
}

func (bp *BatchProverTask) getBatchTask(ctx *gin.Context, getTaskParameter *coordinatorType.GetTaskParameter) (*orm.Batch, error) {
	maxActiveAttempts := bp.cfg.ProverManager.ProversPerSession
	maxTotalAttempts := bp.cfg.ProverManager.SessionAttempts

	for i := 0; i < 5; i++ {
		batchTask, err := bp.batchOrm.GetAssignedBatch(ctx.Copy(), maxActiveAttempts, maxTotalAttempts)
		if err != nil {
			log.Error("failed to get assigned batch proving tasks", "height", getTaskParameter.ProverHeight, "err", err)
			return nil, ErrCoordinatorInternalFailure
		}

		if batchTask == nil {
			batchTask, err = bp.batchOrm.GetUnassignedBatch(ctx.Copy(), maxActiveAttempts, maxTotalAttempts)
			if err != nil {
				log.Error("failed to get unassigned batch proving tasks", "height", getTaskParameter.ProverHeight, "err", err)
				return nil, ErrCoordinatorInternalFailure
			}
		}

		if batchTask != nil {
			rowsAffected, err := bp.batchOrm.UpdateBatchAttempts(ctx.Copy(), batchTask.Index, batchTask.ActiveAttempts, batchTask.TotalAttempts)
			if err != nil || rowsAffected == 0 {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return batchTask, nil
		}
	}

	log.Debug("get empty unassigned batch after retry 5 times", "height", getTaskParameter.ProverHeight)
	return nil, nil
}

func (bp *BatchProverTask) recordMetrics(hardForkName string, proverTask *orm.ProverTask) {
	bp.batchTaskGetTaskTotal.WithLabelValues(hardForkName).Inc()
	bp.batchTaskGetTaskProver.With(prometheus.Labels{
		coordinatorType.LabelProverName:      proverTask.ProverName,
		coordinatorType.LabelProverPublicKey: proverTask.ProverPublicKey,
		coordinatorType.LabelProverVersion:   proverTask.ProverVersion,
	}).Inc()
}

// Other methods (hardForkName, formatProverTask, recoverActiveAttempts, getBatchTaskDetail) remain unchanged
