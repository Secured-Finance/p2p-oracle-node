package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Secured-Finance/dione/pubsub"

	"github.com/Secured-Finance/dione/consensus/policy"

	"github.com/wealdtech/go-merkletree/keccak256"

	"github.com/wealdtech/go-merkletree"

	"github.com/sirupsen/logrus"

	"github.com/Secured-Finance/dione/node/wire"

	"github.com/libp2p/go-libp2p-core/peer"

	types2 "github.com/Secured-Finance/dione/blockchain/types"

	"github.com/Secured-Finance/dione/blockchain/pool"
	gorpc "github.com/libp2p/go-libp2p-gorpc"
)

type SyncManager interface {
	Start()
	Stop()
}

type syncManager struct {
	blockpool            *pool.BlockPool
	mempool              *pool.Mempool
	wg                   sync.WaitGroup
	ctx                  context.Context
	ctxCancelFunc        context.CancelFunc
	initialSyncCompleted bool
	bootstrapPeer        peer.ID
	rpcClient            *gorpc.Client
	psb                  *pubsub.PubSubRouter
}

func NewSyncManager(bp *pool.BlockPool, mp *pool.Mempool, p2pRPCClient *gorpc.Client, bootstrapPeer peer.ID, psb *pubsub.PubSubRouter) SyncManager {
	ctx, cancelFunc := context.WithCancel(context.Background())
	sm := &syncManager{
		blockpool:            bp,
		mempool:              mp,
		ctx:                  ctx,
		ctxCancelFunc:        cancelFunc,
		initialSyncCompleted: false,
		bootstrapPeer:        bootstrapPeer,
		rpcClient:            p2pRPCClient,
		psb:                  psb,
	}

	psb.Hook(pubsub.NewTxMessageType, sm.onNewTransaction, types2.Transaction{})

	return sm
}

func (sm *syncManager) Start() {
	sm.wg.Add(1)

	err := sm.doInitialBlockPoolSync()
	if err != nil {
		logrus.Error(err)
	}

	err = sm.doInitialMempoolSync()
	if err != nil {
		logrus.Error(err)
	}

	go sm.syncLoop()
}

func (sm *syncManager) Stop() {
	sm.ctxCancelFunc()
	sm.wg.Wait()
}

func (sm *syncManager) doInitialBlockPoolSync() error {
	if sm.initialSyncCompleted {
		return nil
	}

	ourLastHeight, err := sm.blockpool.GetLatestBlockHeight()
	if err == pool.ErrLatestHeightNil {
		gBlock := types2.GenesisBlock()
		err = sm.blockpool.StoreBlock(gBlock) // commit genesis block
		if err != nil {
			return err
		}
	}

	var reply wire.LastBlockHeightReply
	err = sm.rpcClient.Call(sm.bootstrapPeer, "NetworkService", "LastBlockHeight", nil, &reply)
	if err != nil {
		return err
	}
	if reply.Error != nil {
		return reply.Error
	}

	if reply.Height > ourLastHeight {
		heightCount := reply.Height - ourLastHeight
		var from uint64
		to := ourLastHeight
		var receivedBlocks []types2.Block
		for heightCount > 0 {
			from = to + 1
			var addedVal uint64
			if heightCount < policy.MaxBlockCountForRetrieving {
				addedVal = heightCount
			} else {
				addedVal = policy.MaxBlockCountForRetrieving
			}
			heightCount -= addedVal
			to += addedVal
			var getBlocksReply wire.GetRangeOfBlocksReply
			arg := wire.GetRangeOfBlocksArg{From: from, To: to}
			err = sm.rpcClient.Call(sm.bootstrapPeer, "NetworkService", "GetRangeOfBlocks", arg, &getBlocksReply)
			if err != nil {
				return err
			}
			receivedBlocks = append(receivedBlocks, getBlocksReply.Blocks...)
			if len(getBlocksReply.FailedBlockHeights) != 0 {
				logrus.Warnf("remote node is unable to retrieve block heights: %s", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(getBlocksReply.FailedBlockHeights)), ", "), "[]"))
				// FIXME we definitely need to handle it, because in that case our chain isn't complete!
			}
		}
		for _, b := range receivedBlocks {
			err := sm.processReceivedBlock(b) // it should process the block synchronously
			if err != nil {
				logrus.Warnf("unable to process block %d: %s", b.Header.Height, err.Error())
				continue
			}
		}
	} else {
		// FIXME probably we need to pick up better peer for syncing, because chain of current peer can be out-of-date as well
	}

	return nil
}

func (sm *syncManager) doInitialMempoolSync() error {
	var reply wire.InvMessage
	err := sm.rpcClient.Call(sm.bootstrapPeer, "NetworkService", "Mempool", nil, &reply)
	if err != nil {
		return err
	}

	var txsToRetrieve [][]byte

	for _, v := range reply.Inventory {
		_, err = sm.mempool.GetTransaction(v.Hash)
		if errors.Is(err, pool.ErrTxNotFound) {
			txsToRetrieve = append(txsToRetrieve, v.Hash)
		}
	}

	for {
		var txHashes [][]byte

		if len(txsToRetrieve) == 0 {
			break
		}

		if len(txsToRetrieve) > policy.MaxTransactionCountForRetrieving {
			txHashes = txsToRetrieve[:policy.MaxTransactionCountForRetrieving]
			txsToRetrieve = txsToRetrieve[policy.MaxTransactionCountForRetrieving:]
		} else {
			txHashes = txsToRetrieve
		}

		getMempoolTxArg := wire.GetMempoolTxsArg{
			Items: txHashes,
		}
		var getMempoolTxReply wire.GetMempoolTxsReply
		err := sm.rpcClient.Call(sm.bootstrapPeer, "NetworkService", "GetMempoolTxs", getMempoolTxArg, &getMempoolTxReply)
		if err != nil {
			return err
		}
		if getMempoolTxReply.Error != nil {
			return getMempoolTxReply.Error
		}
		for _, v := range getMempoolTxReply.Transactions {
			err := sm.mempool.StoreTx(&v)
			if err != nil {
				logrus.Warnf(err.Error())
			}
		}
		// FIXME handle not found transactions
	}

	return nil
}

func (sm *syncManager) processReceivedBlock(block types2.Block) error {
	// validate block
	previousBlockHeader, err := sm.blockpool.FetchBlockHeaderByHeight(block.Header.Height - 1)
	if err != nil {
		return fmt.Errorf("failed to retrieve previous block %d", block.Header.Height-1)
	}
	if bytes.Compare(block.Header.LastHash, previousBlockHeader.Hash) != 0 {
		return fmt.Errorf("block header has invalid last block hash")
	}
	verified, err := merkletree.VerifyProofUsing(previousBlockHeader.Hash, false, block.Header.LastHashProof, [][]byte{block.Header.Hash}, keccak256.New())
	if err != nil {
		return fmt.Errorf("failed to verify last block hash merkle proof: %s", err.Error())
	}
	if !verified {
		return fmt.Errorf("merkle hash of current block doesn't contain hash of previous block")
	}

	// check if hashes of block transactions are present in the block hash merkle tree
	for _, tx := range block.Data { // FIXME we need to do something with rejected txs
		if tx.MerkleProof == nil {
			return fmt.Errorf("block transaction hasn't merkle proof")
		}
		txProofVerified, err := merkletree.VerifyProofUsing(tx.Hash, false, tx.MerkleProof, [][]byte{block.Header.Hash}, keccak256.New())
		if err != nil {
			return fmt.Errorf("failed to verify tx hash merkle proof: %s", err.Error())
		}
		if !txProofVerified {
			return fmt.Errorf("transaction doesn't present in block hash merkle tree")
		}
		if !tx.ValidateHash() {
			return fmt.Errorf("transaction hash is invalid")
		}
	}

	err = sm.blockpool.StoreBlock(&block)
	if err != nil {
		return fmt.Errorf("failed to store block in blockpool: %s", err.Error())
	}

	return nil
}

func (sm *syncManager) syncLoop() {
}

func (sm *syncManager) onNewTransaction(message *pubsub.GenericMessage) {
	tx, ok := message.Payload.(types2.Transaction)
	if !ok {
		logrus.Warn("failed to convert payload to Transaction")
		return
	}

	if !tx.ValidateHash() {
		logrus.Warn("failed to validate tx hash, rejecting it")
		return
	} // TODO add more checks on tx

	err := sm.mempool.StoreTx(&tx)
	if err != nil {
		logrus.Warnf("failed to store incoming transaction in mempool: %s", err.Error())
	}
}
