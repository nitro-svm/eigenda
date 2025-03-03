package encoding

import (
	"crypto/sha256"

	"github.com/Layr-Labs/eigenda/core"
	"github.com/Layr-Labs/eigenda/encoding"
	"github.com/Layr-Labs/eigenda/encoding/kzgrs"
	"github.com/Layr-Labs/eigenda/encoding/kzgrs/prover"
	"github.com/Layr-Labs/eigenda/encoding/kzgrs/verifier"
	encoder "github.com/Layr-Labs/eigenda/encoding/rs"
	"github.com/Layr-Labs/eigenda/pkg/kzg/bn254"
	lru "github.com/hashicorp/golang-lru/v2"
)

func toEncParams(params core.EncodingParams) encoder.EncodingParams {
	return encoder.ParamsFromMins(uint64(params.NumChunks), uint64(params.ChunkLength))
}

type EncoderConfig struct {
	KzgConfig         kzgrs.KzgConfig
	CacheEncodedBlobs bool
}

type Encoder struct {
	Config        EncoderConfig
	EncoderGroup  *prover.Prover
	VerifierGroup *verifier.Verifier
	Cache         *lru.Cache[string, encodedValue]
}

var _ core.Encoder = &Encoder{}

func NewEncoder(config EncoderConfig, loadG2Points bool) (*Encoder, error) {
	kzgEncoderGroup, err := prover.NewProver(&config.KzgConfig, loadG2Points)
	if err != nil {
		return nil, err
	}

	kzgVerifierGroup, err := verifier.NewVerifier(&config.KzgConfig, loadG2Points)
	if err != nil {
		return nil, err
	}

	cache, err := lru.New[string, encodedValue](128)
	if err != nil {
		return nil, err
	}

	return &Encoder{
		EncoderGroup:  kzgEncoderGroup,
		VerifierGroup: kzgVerifierGroup,
		Cache:         cache,
		Config:        config,
	}, nil
}

type encodedValue struct {
	commitments core.BlobCommitments
	chunks      []*core.Chunk
	err         error
}

func (e *Encoder) Encode(data []byte, params core.EncodingParams) (core.BlobCommitments, []*core.Chunk, error) {

	var cacheKey string = ""
	if e.Config.CacheEncodedBlobs {
		cacheKey = hashBlob(data, params)
		if v, ok := e.Cache.Get(cacheKey); ok {
			return v.commitments, v.chunks, v.err
		}
	}
	encParams := toEncParams(params)

	enc, err := e.EncoderGroup.GetKzgEncoder(encParams)
	if err != nil {
		return core.BlobCommitments{}, nil, err
	}

	commit, lowDegreeCommit, lowDegreeProof, kzgFrames, _, err := enc.EncodeBytes(data)
	if err != nil {
		return core.BlobCommitments{}, nil, err
	}

	chunks := make([]*core.Chunk, len(kzgFrames))
	for ind, frame := range kzgFrames {

		chunks[ind] = &core.Chunk{
			Coeffs: frame.Coeffs,
			Proof:  frame.Proof,
		}
	}

	length := uint(len(encoder.ToFrArray(data)))
	commitments := core.BlobCommitments{
		Commitment:       (*core.G1Commitment)(commit),
		LengthCommitment: (*core.G2Commitment)(lowDegreeCommit),
		LengthProof:      (*core.G2Commitment)(lowDegreeProof),
		Length:           length,
	}

	if e.Config.CacheEncodedBlobs {
		e.Cache.Add(cacheKey, encodedValue{
			commitments: commitments,
			chunks:      chunks,
			err:         nil,
		})
	}
	return commitments, chunks, nil
}

func (e *Encoder) VerifyBlobLength(commitments core.BlobCommitments) error {
	return e.VerifierGroup.VerifyCommit((*bn254.G2Point)(commitments.LengthCommitment), (*bn254.G2Point)(commitments.LengthProof), uint64(commitments.Length))

}

func (e *Encoder) VerifyChunks(chunks []*core.Chunk, indices []core.ChunkNumber, commitments core.BlobCommitments, params core.EncodingParams) error {

	encParams := toEncParams(params)

	verifier, err := e.VerifierGroup.GetKzgVerifier(encParams)
	if err != nil {
		return err
	}

	for ind := range chunks {
		err = verifier.VerifyFrame(
			(*bn254.G1Point)(commitments.Commitment),
			&encoding.Frame{
				Proof:  chunks[ind].Proof,
				Coeffs: chunks[ind].Coeffs,
			},
			uint64(indices[ind]),
		)

		if err != nil {
			return err
		}
	}

	return nil

}

func (e *Encoder) VerifyCommitEquivalenceBatch(commitments []core.BlobCommitments) error {
	commitmentsPair := make([]verifier.CommitmentPair, len(commitments))

	for i, c := range commitments {
		commitmentsPair[i] = verifier.CommitmentPair{
			Commitment:       (bn254.G1Point)(*c.Commitment),
			LengthCommitment: (bn254.G2Point)(*c.LengthCommitment),
		}
	}
	return e.VerifierGroup.BatchVerifyCommitEquivalence(commitmentsPair)
}

// convert struct understandable by the crypto library
func (e *Encoder) UniversalVerifySubBatch(params core.EncodingParams, samplesCore []core.Sample, numBlobs int) error {
	encParams := toEncParams(params)
	samples := make([]verifier.Sample, len(samplesCore))

	for i, sc := range samplesCore {
		x, err := encoder.GetLeadingCosetIndex(
			uint64(sc.AssignmentIndex),
			encParams.NumChunks,
		)
		if err != nil {
			return err
		}

		sample := verifier.Sample{
			Commitment: (bn254.G1Point)(*sc.Commitment),
			Proof:      sc.Chunk.Proof,
			RowIndex:   sc.BlobIndex,
			Coeffs:     sc.Chunk.Coeffs,
			X:          uint(x),
		}
		samples[i] = sample
	}

	return e.VerifierGroup.UniversalVerify(encParams, samples, numBlobs)
}

// Decode takes in the chunks, indices, and encoding parameters and returns the decoded blob
// The result is trimmed to the given maxInputSize.
func (e *Encoder) Decode(chunks []*core.Chunk, indices []core.ChunkNumber, params core.EncodingParams, maxInputSize uint64) ([]byte, error) {
	frames := make([]encoding.Frame, len(chunks))
	for i := range chunks {
		frames[i] = encoding.Frame{
			Proof:  chunks[i].Proof,
			Coeffs: chunks[i].Coeffs,
		}
	}
	encoder, err := e.EncoderGroup.GetKzgEncoder(toEncParams(params))
	if err != nil {
		return nil, err
	}

	return encoder.Decode(frames, toUint64Array(indices), maxInputSize)
}

func toUint64Array(chunkIndices []core.ChunkNumber) []uint64 {
	res := make([]uint64, len(chunkIndices))
	for i, d := range chunkIndices {
		res[i] = uint64(d)
	}
	return res
}

func hashBlob(data []byte, params core.EncodingParams) string {
	h := sha256.New()
	h.Write(data)
	h.Write([]byte{byte(params.ChunkLength), byte(params.NumChunks)})
	return string(h.Sum(nil))
}
