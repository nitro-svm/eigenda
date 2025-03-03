package batcher_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"runtime"
	"testing"
	"time"

	"github.com/Layr-Labs/eigenda/common"
	"github.com/Layr-Labs/eigenda/common/logging"
	cmock "github.com/Layr-Labs/eigenda/common/mock"
	"github.com/Layr-Labs/eigenda/core"
	"github.com/Layr-Labs/eigenda/core/encoding"
	coremock "github.com/Layr-Labs/eigenda/core/mock"
	"github.com/Layr-Labs/eigenda/disperser"
	bat "github.com/Layr-Labs/eigenda/disperser/batcher"
	"github.com/Layr-Labs/eigenda/disperser/batcher/mock"
	batchermock "github.com/Layr-Labs/eigenda/disperser/batcher/mock"
	"github.com/Layr-Labs/eigenda/disperser/common/inmem"
	dmock "github.com/Layr-Labs/eigenda/disperser/mock"
	"github.com/Layr-Labs/eigenda/encoding/kzgrs"
	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/assert"
)

var (
	gettysburgAddressBytes  = []byte("Fourscore and seven years ago our fathers brought forth, on this continent, a new nation, conceived in liberty, and dedicated to the proposition that all men are created equal. Now we are engaged in a great civil war, testing whether that nation, or any nation so conceived, and so dedicated, can long endure. We are met on a great battle-field of that war. We have come to dedicate a portion of that field, as a final resting-place for those who here gave their lives, that that nation might live. It is altogether fitting and proper that we should do this. But, in a larger sense, we cannot dedicate, we cannot consecrate—we cannot hallow—this ground. The brave men, living and dead, who struggled here, have consecrated it far above our poor power to add or detract. The world will little note, nor long remember what we say here, but it can never forget what they did here. It is for us the living, rather, to be dedicated here to the unfinished work which they who fought here have thus far so nobly advanced. It is rather for us to be here dedicated to the great task remaining before us—that from these honored dead we take increased devotion to that cause for which they here gave the last full measure of devotion—that we here highly resolve that these dead shall not have died in vain—that this nation, under God, shall have a new birth of freedom, and that government of the people, by the people, for the people, shall not perish from the earth.")
	handleBatchLivenessChan = make(chan time.Time, 1)
)

type batcherComponents struct {
	transactor       *coremock.MockTransactor
	txnManager       *batchermock.MockTxnManager
	blobStore        disperser.BlobStore
	encoderClient    *disperser.LocalEncoderClient
	encodingStreamer *bat.EncodingStreamer
	ethClient        *cmock.MockEthClient
}

// makeTestEncoder makes an encoder currently using the only supported backend.
func makeTestEncoder() (core.Encoder, error) {
	config := kzgrs.KzgConfig{
		G1Path:          "../../inabox/resources/kzg/g1.point",
		G2Path:          "../../inabox/resources/kzg/g2.point",
		CacheDir:        "../../inabox/resources/kzg/SRSTables",
		SRSOrder:        3000,
		SRSNumberToLoad: 3000,
		NumWorker:       uint64(runtime.GOMAXPROCS(0)),
	}

	return encoding.NewEncoder(encoding.EncoderConfig{KzgConfig: config}, true)
}

func makeTestBlob(securityParams []*core.SecurityParam) core.Blob {
	blob := core.Blob{
		RequestHeader: core.BlobRequestHeader{
			SecurityParams: securityParams,
		},
		Data: gettysburgAddressBytes,
	}
	return blob
}

func makeBatcher(t *testing.T) (*batcherComponents, *bat.Batcher, func() []time.Time) {
	// Common Components
	logger, err := logging.GetLogger(logging.DefaultCLIConfig())
	assert.NoError(t, err)

	// Core Components
	cst, err := coremock.MakeChainDataMock(10)
	assert.NoError(t, err)
	cst.On("GetCurrentBlockNumber").Return(uint(10), nil)
	asgn := &core.StdAssignmentCoordinator{}
	transactor := &coremock.MockTransactor{}
	transactor.On("OperatorIDToAddress").Return(gethcommon.Address{}, nil)
	agg, err := core.NewStdSignatureAggregator(logger, transactor)
	assert.NoError(t, err)
	enc, err := makeTestEncoder()
	assert.NoError(t, err)

	state := cst.GetTotalOperatorState(context.Background(), 0)

	// Disperser Components
	dispatcher := dmock.NewDispatcher(state)
	blobStore := inmem.NewBlobStore()

	pullInterval := 100 * time.Millisecond
	config := bat.Config{
		PullInterval:             pullInterval,
		NumConnections:           1,
		EncodingRequestQueueSize: 100,
		BatchSizeMBLimit:         100,
		SRSOrder:                 3000,
		MaxNumRetriesPerBlob:     2,
	}
	timeoutConfig := bat.TimeoutConfig{
		EncodingTimeout:    10 * time.Second,
		AttestationTimeout: 10 * time.Second,
		ChainReadTimeout:   10 * time.Second,
		ChainWriteTimeout:  10 * time.Second,
	}

	metrics := bat.NewMetrics("9100", logger)

	encoderClient := disperser.NewLocalEncoderClient(enc)
	finalizer := batchermock.NewFinalizer()
	ethClient := &cmock.MockEthClient{}
	txnManager := mock.NewTxnManager()

	b, err := bat.NewBatcher(config, timeoutConfig, blobStore, dispatcher, cst, asgn, encoderClient, agg, ethClient, finalizer, transactor, txnManager, logger, metrics, handleBatchLivenessChan)
	assert.NoError(t, err)

	var heartbeatsReceived []time.Time
	doneListening := make(chan bool)

	go func() {
		for {
			select {
			case hb := <-b.HeartbeatChan:
				heartbeatsReceived = append(heartbeatsReceived, hb)
			case <-doneListening:
				return
			}
		}
	}()

	// Make the batcher
	return &batcherComponents{
			transactor:       transactor,
			txnManager:       txnManager,
			blobStore:        blobStore,
			encoderClient:    encoderClient,
			encodingStreamer: b.EncodingStreamer,
			ethClient:        ethClient,
		}, b, func() []time.Time {
			close(doneListening) // Stop the goroutine listening to heartbeats
			return heartbeatsReceived
		}
}

func queueBlob(t *testing.T, ctx context.Context, blob *core.Blob, blobStore disperser.BlobStore) (uint64, disperser.BlobKey) {
	requestedAt := uint64(time.Now().UnixNano())
	blobKey, err := blobStore.StoreBlob(ctx, blob, requestedAt)
	assert.NoError(t, err)

	return requestedAt, blobKey
}

func TestBatcherIterations(t *testing.T) {
	blob1 := makeTestBlob([]*core.SecurityParam{{
		QuorumID:           0,
		AdversaryThreshold: 80,
		QuorumThreshold:    100,
	}})
	blob2 := makeTestBlob([]*core.SecurityParam{{
		QuorumID:           1,
		AdversaryThreshold: 70,
		QuorumThreshold:    100,
	}})
	components, batcher, getHeartbeats := makeBatcher(t)

	defer func() {
		heartbeats := getHeartbeats()
		assert.NotEmpty(t, heartbeats, "Expected heartbeats, but none were received")

		// Further assertions can be made here, such as checking the number of heartbeats
		// or validating the time intervals between them if needed.
	}()
	// should be encoding 3 and 0
	logData, err := hex.DecodeString("00000000000000000000000000000000000000000000000000000000000000030000000000000000000000000000000000000000000000000000000000000000")
	assert.NoError(t, err)

	txHash := gethcommon.HexToHash("0x1234")
	blockNumber := big.NewInt(123)
	receipt := &types.Receipt{
		Logs: []*types.Log{
			{
				Topics: []gethcommon.Hash{common.BatchConfirmedEventSigHash, gethcommon.HexToHash("1234")},
				Data:   logData,
			},
		},
		BlockNumber: blockNumber,
		TxHash:      txHash,
	}
	blobStore := components.blobStore
	ctx := context.Background()
	requestedAt1, blobKey1 := queueBlob(t, ctx, &blob1, blobStore)
	_, blobKey2 := queueBlob(t, ctx, &blob2, blobStore)

	// Start the batcher
	out := make(chan bat.EncodingResultOrStatus)
	err = components.encodingStreamer.RequestEncoding(ctx, out)
	assert.NoError(t, err)
	err = components.encodingStreamer.ProcessEncodedBlobs(ctx, <-out)
	assert.NoError(t, err)
	err = components.encodingStreamer.ProcessEncodedBlobs(ctx, <-out)
	assert.NoError(t, err)
	count, size := components.encodingStreamer.EncodedBlobstore.GetEncodedResultSize()
	assert.Equal(t, 2, count)
	assert.Equal(t, uint64(197632), size)

	txn := types.NewTransaction(0, gethcommon.Address{}, big.NewInt(0), 0, big.NewInt(0), nil)
	components.transactor.On("BuildConfirmBatchTxn").Return(txn, nil)
	components.txnManager.On("ProcessTransaction").Return(nil)

	err = batcher.HandleSingleBatch(ctx)
	assert.NoError(t, err)
	assert.Greater(t, len(components.txnManager.Requests), 0)
	err = batcher.ProcessConfirmedBatch(ctx, &bat.ReceiptOrErr{
		Receipt:  receipt,
		Err:      nil,
		Metadata: components.txnManager.Requests[len(components.txnManager.Requests)-1].Metadata,
	})
	assert.NoError(t, err)
	// Check that the blob was processed
	meta1, err := blobStore.GetBlobMetadata(ctx, blobKey1)
	assert.NoError(t, err)
	assert.Equal(t, blobKey1, meta1.GetBlobKey())
	assert.Equal(t, requestedAt1, meta1.RequestMetadata.RequestedAt)
	assert.Equal(t, disperser.Confirmed, meta1.BlobStatus)
	assert.Equal(t, meta1.ConfirmationInfo.BatchID, uint32(3))
	assert.Equal(t, meta1.ConfirmationInfo.ConfirmationTxnHash, txHash)
	assert.Equal(t, meta1.ConfirmationInfo.ConfirmationBlockNumber, uint32(blockNumber.Int64()))

	meta2, err := blobStore.GetBlobMetadata(ctx, blobKey2)
	assert.NoError(t, err)
	assert.Equal(t, blobKey2, meta2.GetBlobKey())
	assert.Equal(t, disperser.Confirmed, meta2.BlobStatus)

	res, err := components.encodingStreamer.EncodedBlobstore.GetEncodingResult(meta1.GetBlobKey(), 0)
	assert.ErrorContains(t, err, "no such key")
	assert.Nil(t, res)
	res, err = components.encodingStreamer.EncodedBlobstore.GetEncodingResult(meta2.GetBlobKey(), 1)
	assert.ErrorContains(t, err, "no such key")
	assert.Nil(t, res)
	count, size = components.encodingStreamer.EncodedBlobstore.GetEncodedResultSize()
	assert.Equal(t, 0, count)
	assert.Equal(t, uint64(0), size)

	// confirmed metadata should be immutable and not be updated
	existingBlobIndex := meta1.ConfirmationInfo.BlobIndex
	meta1, err = blobStore.MarkBlobConfirmed(ctx, meta1, &disperser.ConfirmationInfo{
		BlobIndex: existingBlobIndex + 1,
	})
	assert.NoError(t, err)
	// check confirmation info isn't updated
	assert.Equal(t, existingBlobIndex, meta1.ConfirmationInfo.BlobIndex)
	assert.Equal(t, disperser.Confirmed, meta1.BlobStatus)
}

func TestBlobFailures(t *testing.T) {
	blob := makeTestBlob([]*core.SecurityParam{{
		QuorumID:           0,
		AdversaryThreshold: 80,
		QuorumThreshold:    100,
	}})

	components, batcher, getHeartbeats := makeBatcher(t)

	defer func() {
		heartbeats := getHeartbeats()
		assert.Equal(t, 3, len(heartbeats), "Expected heartbeats, but none were received")
	}()

	confirmationErr := fmt.Errorf("error")
	blobStore := components.blobStore
	ctx := context.Background()
	requestedAt, blobKey := queueBlob(t, ctx, &blob, blobStore)

	// Start the batcher
	out := make(chan bat.EncodingResultOrStatus)
	err := components.encodingStreamer.RequestEncoding(ctx, out)
	assert.NoError(t, err)
	err = components.encodingStreamer.ProcessEncodedBlobs(ctx, <-out)
	assert.NoError(t, err)

	txn := types.NewTransaction(0, gethcommon.Address{}, big.NewInt(0), 0, big.NewInt(0), nil)
	components.transactor.On("BuildConfirmBatchTxn").Return(txn, nil)
	components.txnManager.On("ProcessTransaction").Return(nil)

	// Test with receipt response with error
	err = batcher.HandleSingleBatch(ctx)
	assert.NoError(t, err)
	assert.Greater(t, len(components.txnManager.Requests), 0)
	err = batcher.ProcessConfirmedBatch(ctx, &bat.ReceiptOrErr{
		Receipt:  nil,
		Err:      confirmationErr,
		Metadata: components.txnManager.Requests[len(components.txnManager.Requests)-1].Metadata,
	})
	assert.ErrorIs(t, err, confirmationErr)

	meta, err := blobStore.GetBlobMetadata(ctx, blobKey)
	assert.NoError(t, err)
	assert.Equal(t, blobKey, meta.GetBlobKey())
	assert.Equal(t, requestedAt, meta.RequestMetadata.RequestedAt)
	// should be retried
	assert.Equal(t, disperser.Processing, meta.BlobStatus)
	assert.Equal(t, uint(1), meta.NumRetries)
	metadatas, err := blobStore.GetBlobMetadataByStatus(ctx, disperser.Processing)
	assert.NoError(t, err)
	assert.Len(t, metadatas, 1)
	encodedResult, err := components.encodingStreamer.EncodedBlobstore.GetEncodingResult(blobKey, 0)
	assert.Error(t, err)
	assert.Nil(t, encodedResult)

	// Test with receipt response with no block number
	err = components.encodingStreamer.RequestEncoding(ctx, out)
	assert.NoError(t, err)
	err = components.encodingStreamer.ProcessEncodedBlobs(ctx, <-out)
	assert.NoError(t, err)
	components.encodingStreamer.ReferenceBlockNumber = 10
	err = batcher.HandleSingleBatch(ctx)
	assert.NoError(t, err)
	err = batcher.ProcessConfirmedBatch(ctx, &bat.ReceiptOrErr{
		Receipt: &types.Receipt{
			TxHash: gethcommon.HexToHash("0x1234"),
		},
		Err:      nil,
		Metadata: components.txnManager.Requests[len(components.txnManager.Requests)-1].Metadata,
	})
	assert.ErrorContains(t, err, "error getting transaction receipt block number")

	meta, err = blobStore.GetBlobMetadata(ctx, blobKey)
	assert.NoError(t, err)

	// should be retried again
	assert.Equal(t, disperser.Processing, meta.BlobStatus)
	assert.Equal(t, uint(2), meta.NumRetries)

	// Try again
	err = components.encodingStreamer.RequestEncoding(ctx, out)
	assert.NoError(t, err)
	err = components.encodingStreamer.ProcessEncodedBlobs(ctx, <-out)
	assert.NoError(t, err)
	components.encodingStreamer.ReferenceBlockNumber = 10
	err = batcher.HandleSingleBatch(ctx)
	assert.NoError(t, err)

	err = batcher.ProcessConfirmedBatch(ctx, &bat.ReceiptOrErr{
		Receipt: &types.Receipt{
			TxHash: gethcommon.HexToHash("0x1234"),
		},
		Err:      nil,
		Metadata: components.txnManager.Requests[len(components.txnManager.Requests)-1].Metadata,
	})
	assert.ErrorContains(t, err, "error getting transaction receipt block number")

	meta, err = blobStore.GetBlobMetadata(ctx, blobKey)
	assert.NoError(t, err)

	// should not be retried again
	assert.Equal(t, disperser.Failed, meta.BlobStatus)
	assert.Equal(t, uint(2), meta.NumRetries)
}

// TestBlobRetry tests that the blob that has been dispersed to DA nodes but is pending onchain confirmation isn't re-dispersed.
func TestBlobRetry(t *testing.T) {
	blob := makeTestBlob([]*core.SecurityParam{{
		QuorumID:           0,
		AdversaryThreshold: 80,
		QuorumThreshold:    100,
	}})

	components, batcher, getHeartbeats := makeBatcher(t)

	defer func() {
		heartbeats := getHeartbeats()
		assert.Equal(t, 1, len(heartbeats), "Expected heartbeats, but none were received")
	}()

	blobStore := components.blobStore
	ctx := context.Background()
	_, blobKey := queueBlob(t, ctx, &blob, blobStore)

	// Start the batcher
	out := make(chan bat.EncodingResultOrStatus)
	err := components.encodingStreamer.RequestEncoding(ctx, out)
	assert.NoError(t, err)
	err = components.encodingStreamer.ProcessEncodedBlobs(ctx, <-out)
	assert.NoError(t, err)

	encodedResult, err := components.encodingStreamer.EncodedBlobstore.GetEncodingResult(blobKey, 0)
	assert.NoError(t, err)
	assert.Equal(t, encodedResult.Status, bat.PendingDispersal)

	txn := types.NewTransaction(0, gethcommon.Address{}, big.NewInt(0), 0, big.NewInt(0), nil)
	components.transactor.On("BuildConfirmBatchTxn").Return(txn, nil)
	components.txnManager.On("ProcessTransaction").Return(nil)

	err = batcher.HandleSingleBatch(ctx)
	assert.NoError(t, err)

	meta, err := blobStore.GetBlobMetadata(ctx, blobKey)
	assert.NoError(t, err)
	assert.Equal(t, disperser.Processing, meta.BlobStatus)
	encodedResult, err = components.encodingStreamer.EncodedBlobstore.GetEncodingResult(blobKey, 0)
	assert.NoError(t, err)
	assert.Equal(t, encodedResult.Status, bat.PendingConfirmation)

	err = components.encodingStreamer.RequestEncoding(ctx, out)
	assert.NoError(t, err)
	timer := time.NewTimer(1 * time.Second)
	select {
	case <-out:
		t.Fatal("shouldn't have picked up any blobs to encode")
	case <-timer.C:
	}
	batch, err := components.encodingStreamer.CreateBatch()
	assert.ErrorContains(t, err, "no encoded results")
	assert.Nil(t, batch)

	// Shouldn't pick up any blobs to encode
	components.encodingStreamer.ReferenceBlockNumber = 12
	err = components.encodingStreamer.RequestEncoding(ctx, out)
	assert.NoError(t, err)
	timer = time.NewTimer(1 * time.Second)
	select {
	case <-out:
		t.Fatal("shouldn't have picked up any blobs to encode")
	case <-timer.C:
	}

	batch, err = components.encodingStreamer.CreateBatch()
	assert.ErrorContains(t, err, "no encoded results")
	assert.Nil(t, batch)
	_, err = components.encodingStreamer.EncodedBlobstore.GetEncodingResult(blobKey, 0)
	assert.NoError(t, err)

	meta, err = blobStore.GetBlobMetadata(ctx, blobKey)
	assert.NoError(t, err)
	assert.Equal(t, disperser.Processing, meta.BlobStatus)

	// Trigger a retry
	confirmationErr := fmt.Errorf("error")
	err = batcher.ProcessConfirmedBatch(ctx, &bat.ReceiptOrErr{
		Receipt:  nil,
		Err:      confirmationErr,
		Metadata: components.txnManager.Requests[len(components.txnManager.Requests)-1].Metadata,
	})
	assert.ErrorIs(t, err, confirmationErr)

	components.encodingStreamer.ReferenceBlockNumber = 14
	// Should pick up the blob to encode
	err = components.encodingStreamer.RequestEncoding(ctx, out)
	assert.NoError(t, err)
	timer = time.NewTimer(1 * time.Second)
	var res bat.EncodingResultOrStatus
	select {
	case res = <-out:
	case <-timer.C:
		t.Fatal("should have picked up the blob to encode")
	}
	err = components.encodingStreamer.ProcessEncodedBlobs(ctx, res)
	assert.NoError(t, err)
	encodedResult, err = components.encodingStreamer.EncodedBlobstore.GetEncodingResult(blobKey, 0)
	assert.NoError(t, err)
	assert.Equal(t, encodedResult.Status, bat.PendingDispersal)
}

func TestRetryTxnReceipt(t *testing.T) {
	var err error
	blob := makeTestBlob([]*core.SecurityParam{{
		QuorumID:           0,
		AdversaryThreshold: 80,
		QuorumThreshold:    100,
	}})
	components, batcher, getHeartbeats := makeBatcher(t)

	defer func() {
		heartbeats := getHeartbeats()
		assert.NotEmpty(t, heartbeats, "Expected heartbeats, but none were received")

		// Further assertions can be made here, such as checking the number of heartbeats
		// or validating the time intervals between them if needed.
	}()
	invalidReceipt := &types.Receipt{
		Logs: []*types.Log{
			{
				Topics: []gethcommon.Hash{common.BatchConfirmedEventSigHash, gethcommon.HexToHash("1234")},
				Data:   []byte{}, // empty data
			},
		},
		BlockNumber: big.NewInt(123),
	}
	// should be encoding 3 and 0
	validLogData, err := hex.DecodeString("00000000000000000000000000000000000000000000000000000000000000030000000000000000000000000000000000000000000000000000000000000000")
	assert.NoError(t, err)
	validReceipt := &types.Receipt{
		Logs: []*types.Log{
			{
				Topics: []gethcommon.Hash{common.BatchConfirmedEventSigHash, gethcommon.HexToHash("1234")},
				Data:   validLogData,
			},
		},
		BlockNumber: big.NewInt(123),
	}

	components.ethClient.On("TransactionReceipt").Return(invalidReceipt, nil).Twice()
	components.ethClient.On("TransactionReceipt").Return(validReceipt, nil).Once()
	blobStore := components.blobStore
	ctx := context.Background()
	requestedAt, blobKey := queueBlob(t, ctx, &blob, blobStore)

	// Start the batcher
	out := make(chan bat.EncodingResultOrStatus)
	err = components.encodingStreamer.RequestEncoding(ctx, out)
	assert.NoError(t, err)
	err = components.encodingStreamer.ProcessEncodedBlobs(ctx, <-out)
	assert.NoError(t, err)

	txn := types.NewTransaction(0, gethcommon.Address{}, big.NewInt(0), 0, big.NewInt(0), nil)
	components.transactor.On("BuildConfirmBatchTxn").Return(txn, nil)
	components.txnManager.On("ProcessTransaction").Return(nil)

	err = batcher.HandleSingleBatch(ctx)
	assert.NoError(t, err)
	err = batcher.ProcessConfirmedBatch(ctx, &bat.ReceiptOrErr{
		Receipt:  invalidReceipt,
		Err:      nil,
		Metadata: components.txnManager.Requests[len(components.txnManager.Requests)-1].Metadata,
	})
	assert.NoError(t, err)
	// Check that the blob was processed
	meta, err := blobStore.GetBlobMetadata(ctx, blobKey)
	assert.NoError(t, err)
	assert.Equal(t, blobKey, meta.GetBlobKey())
	assert.Equal(t, requestedAt, meta.RequestMetadata.RequestedAt)
	assert.Equal(t, disperser.Confirmed, meta.BlobStatus)
	assert.Equal(t, meta.ConfirmationInfo.BatchID, uint32(3))
	components.ethClient.AssertNumberOfCalls(t, "TransactionReceipt", 3)
}
