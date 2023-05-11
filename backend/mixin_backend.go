package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"os"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/jsvisa/jsonrpc/ethclient"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type mixinBackend struct {
	chain string
	ec    *ethclient.Client
	db    *gorm.DB
	bc    *lru.Cache[int64, uint64]
}

const (
	blockCacheLimit = 90000
)

var (
	gormLogger = logger.New(
		log.New(os.Stdout, "\r\n", log.LstdFlags), // io writer
		logger.Config{
			SlowThreshold:             10 * time.Millisecond, // Slow SQL threshold
			LogLevel:                  logger.Info,           // Log level
			IgnoreRecordNotFoundError: true,                  // Ignore ErrRecordNotFound error for logger
			ParameterizedQueries:      false,                 // Don't include params in the SQL log
			Colorful:                  true,                  // Disable color
		},
	)
)

func NewMixinBackend(ctx context.Context, chain string, rawurl string, dbdsn string) (*mixinBackend, error) {
	ec, err := ethclient.DialContext(ctx, rawurl)
	if err != nil {
		return nil, err
	}

	dialect := postgres.New(postgres.Config{DSN: dbdsn, PreferSimpleProtocol: true})
	db, err := gorm.Open(dialect, &gorm.Config{TranslateError: true, Logger: gormLogger})
	if err != nil {
		return nil, err
	}

	b := &mixinBackend{
		chain: chain,
		ec:    ec,
		db:    db,
		bc:    lru.NewCache[int64, uint64](blockCacheLimit),
	}
	return b, nil
}

func (b *mixinBackend) HeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Header, error) {
	block, err := b.ec.BlockByNumber(ctx, big.NewInt(number.Int64()))
	if err != nil {
		return nil, err
	}
	return block.Header(), nil
}

func (b *mixinBackend) BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Block, error) {
	return b.ec.BlockByNumber(ctx, big.NewInt(number.Int64()))
}

func (b *mixinBackend) BlockTimestamp(ctx context.Context, number rpc.BlockNumber) (uint64, error) {
	blknum := big.NewInt(number.Int64())
	if cached, ok := b.bc.Get(number.Int64()); ok {
		return cached, nil
	}
	block, err := b.ec.BlockByNumber(ctx, blknum)
	if err != nil {
		return 0, err
	}
	b.bc.Add(number.Int64(), block.Time())
	return block.Time(), nil
}

type rpcTransaction struct {
	tx *types.Transaction
	txExtraInfo
}

type txExtraInfo struct {
	BlockNumber *string         `json:"blockNumber,omitempty"`
	BlockHash   *common.Hash    `json:"blockHash,omitempty"`
	From        *common.Address `json:"from,omitempty"`
}

func (tx *rpcTransaction) UnmarshalJSON(msg []byte) error {
	if err := json.Unmarshal(msg, &tx.tx); err != nil {
		return err
	}
	return json.Unmarshal(msg, &tx.txExtraInfo)
}

func (b *mixinBackend) TransactionByHash(ctx context.Context, txHash common.Hash) (tx *types.Transaction, number uint64, time uint64, err error) {
	var resp *rpcTransaction
	err = b.ec.Client().CallContext(ctx, &resp, "eth_getTransactionByHash", txHash)
	if err != nil {
		return
	} else if resp == nil {
		err = ethereum.NotFound
		return
	}
	blknum := resp.BlockNumber
	if blknum == nil {
		return nil, 0, 0, fmt.Errorf("transaction is pending")
	}
	number = hexutil.MustDecodeUint64(*blknum)
	time, err = b.BlockTimestamp(ctx, rpc.BlockNumber(number))
	if err != nil {
		return
	}
	return resp.tx, number, time, nil
}

func (b *mixinBackend) TraceBlock(ctx context.Context, number rpc.BlockNumber) ([]*CallFrame, error) {
	blockTime, err := b.BlockTimestamp(ctx, number)
	if err != nil {
		return nil, err
	}
	return b.trace(ctx, b.db.Where("block_timestamp = ?", time.Unix(int64(blockTime), 0)).Where("blknum = ?", number.Int64()))
}

func (b *mixinBackend) TraceTransaction(ctx context.Context, txHash common.Hash) ([]*CallFrame, error) {
	_, number, blockTime, err := b.TransactionByHash(ctx, txHash)
	if err != nil {
		return nil, err
	}
	return b.trace(ctx, b.db.Where("block_timestamp = ?", time.Unix(int64(blockTime), 0)).Where("blknum = ?", number).Where("txhash = ?", txHash.Hex()))
}

func (b *mixinBackend) trace(ctx context.Context, sql *gorm.DB) ([]*CallFrame, error) {
	var traces []Trace
	err := sql.
		Table(fmt.Sprintf("%s.%s", b.chain, "traces")).
		Find(&traces).
		Order("txpos ASC, trace_address ASC").
		Error
	if err != nil {
		return nil, err
	}

	callFrames := make([]*CallFrame, len(traces))
	for i, trace := range traces {
		callFrames[i] = trace.AsCallFrame()
	}
	return callFrames, nil
}
