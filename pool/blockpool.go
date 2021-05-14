package pool

import (
	"encoding/hex"
	"errors"

	"github.com/Secured-Finance/dione/types"
	"github.com/fxamacker/cbor/v2"
	"github.com/ledgerwatch/lmdb-go/lmdb"
)

const (
	DefaultBlockPrefix       = "block_"
	DefaultBlockHeaderPrefix = "header_"
)

var (
	ErrBlockNotFound = errors.New("block isn't found")
)

type BlockPool struct {
	dbEnv *lmdb.Env
	db    lmdb.DBI
}

func NewBlockPool(path string) (*BlockPool, error) {
	pool := &BlockPool{}

	// configure lmdb env
	env, err := lmdb.NewEnv()
	if err != nil {
		return nil, err
	}

	err = env.SetMapSize(100 * 1024 * 1024 * 1024) // 100 GB
	if err != nil {
		return nil, err
	}

	err = env.Open(path, 0, 0664)
	if err != nil {
		return nil, err
	}

	pool.dbEnv = env

	var dbi lmdb.DBI
	err = env.Update(func(txn *lmdb.Txn) error {
		dbi, err = txn.OpenDBI("blocks", lmdb.Create)
		return err
	})
	if err != nil {
		return nil, err
	}

	pool.db = dbi

	return pool, nil
}

func (bp *BlockPool) StoreBlock(block *types.Block) error {
	return bp.dbEnv.Update(func(txn *lmdb.Txn) error {
		data, err := cbor.Marshal(block)
		if err != nil {
			return err
		}
		headerData, err := cbor.Marshal(block.Header)
		if err != nil {
			return err
		}
		blockHash := hex.EncodeToString(block.Header.Hash)
		err = txn.Put(bp.db, []byte(DefaultBlockPrefix+blockHash), data, 0)
		if err != nil {
			return err
		}
		err = txn.Put(bp.db, []byte(DefaultBlockHeaderPrefix+blockHash), headerData, 0) // store header separately for easy fetching
		return err
	})
}

func (bp *BlockPool) HasBlock(blockHash string) (bool, error) {
	var blockExists bool
	err := bp.dbEnv.View(func(txn *lmdb.Txn) error {
		_, err := txn.Get(bp.db, []byte(DefaultBlockPrefix+blockHash)) // try to fetch block header
		if err != nil {
			if lmdb.IsNotFound(err) {
				blockExists = false
				return nil
			}
			return err
		}
		blockExists = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return blockExists, nil
}

func (bp *BlockPool) FetchBlock(blockHash string) (*types.Block, error) {
	var block types.Block
	err := bp.dbEnv.View(func(txn *lmdb.Txn) error {
		data, err := txn.Get(bp.db, []byte(DefaultBlockPrefix+blockHash))
		if err != nil {
			if lmdb.IsNotFound(err) {
				return ErrBlockNotFound
			}
			return err
		}
		err = cbor.Unmarshal(data, &block)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &block, nil
}

func (bp *BlockPool) FetchBlockHeader(blockHash string) (*types.BlockHeader, error) {
	var blockHeader types.BlockHeader
	err := bp.dbEnv.View(func(txn *lmdb.Txn) error {
		data, err := txn.Get(bp.db, []byte(DefaultBlockHeaderPrefix+blockHash))
		if err != nil {
			if lmdb.IsNotFound(err) {
				return ErrBlockNotFound
			}
			return err
		}
		err = cbor.Unmarshal(data, &blockHeader)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &blockHeader, nil
}