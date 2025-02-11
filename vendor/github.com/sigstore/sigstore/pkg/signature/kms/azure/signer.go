//
// Copyright 2021 The Sigstore Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package azure

import (
	"bytes"
	"context"
	"crypto"
	"io"
	"math/big"

	"github.com/pkg/errors"
	"golang.org/x/crypto/cryptobyte"
	"golang.org/x/crypto/cryptobyte/asn1"

	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/sigstore/sigstore/pkg/signature/options"
)

var azureSupportedHashFuncs = []crypto.Hash{
	crypto.SHA256,
}

//nolint:golint
const (
	Algorithm_ES256 = "ES256"
)

var azureSupportedAlgorithms []string = []string{
	Algorithm_ES256,
}

type SignerVerifier struct {
	defaultCtx context.Context
	hashFunc   crypto.Hash
	client     *azureVaultClient
}

// LoadSignerVerifier generates signatures using the specified key object in GCP KMS and hash algorithm.
//
// It also can verify signatures locally using the public key. hashFunc must not be crypto.Hash(0).
func LoadSignerVerifier(defaultCtx context.Context, referenceStr string, hashFunc crypto.Hash) (*SignerVerifier, error) {
	a := &SignerVerifier{
		defaultCtx: defaultCtx,
	}

	var err error
	a.client, err = newAzureKMS(defaultCtx, referenceStr)
	if err != nil {
		return nil, err
	}

	switch hashFunc {
	case 0, crypto.SHA224, crypto.SHA256, crypto.SHA384, crypto.SHA512:
		a.hashFunc = hashFunc
	default:
		return nil, errors.New("hash function not supported by Hashivault")
	}

	return a, nil
}

// THIS WILL BE REMOVED ONCE ALL SIGSTORE PROJECTS NO LONGER USE IT
func (a *SignerVerifier) Sign(ctx context.Context, payload []byte) ([]byte, []byte, error) {
	sig, err := a.SignMessage(bytes.NewReader(payload), options.WithContext(ctx))
	return sig, nil, err
}

// SignMessage signs the provided message using GCP KMS. If the message is provided,
// this method will compute the digest according to the hash function specified
// when the Signer was created.
//
// SignMessage recognizes the following Options listed in order of preference:
//
// - WithContext()
//
// - WithDigest()
//
// - WithCryptoSignerOpts()
//
// All other options are ignored if specified.
func (a *SignerVerifier) SignMessage(message io.Reader, opts ...signature.SignOption) ([]byte, error) {
	ctx := context.Background()
	var digest []byte
	var signerOpts crypto.SignerOpts = a.hashFunc

	for _, opt := range opts {
		opt.ApplyDigest(&digest)
		opt.ApplyCryptoSignerOpts(&signerOpts)
	}

	digest, _, err := signature.ComputeDigestForSigning(message, signerOpts.HashFunc(), azureSupportedHashFuncs, opts...)
	if err != nil {
		return nil, err
	}

	rawSig, err := a.client.sign(ctx, digest)
	if err != nil {
		return nil, err
	}

	l := len(rawSig)
	r, s := &big.Int{}, &big.Int{}
	r.SetBytes(rawSig[0 : l/2])
	s.SetBytes(rawSig[l/2:])

	// Convert the concantenated r||s byte string to an ASN.1 sequence
	// This logic is borrowed from https://cs.opensource.google/go/go/+/refs/tags/go1.17.3:src/crypto/ecdsa/ecdsa.go;l=121
	var b cryptobyte.Builder
	b.AddASN1(asn1.SEQUENCE, func(b *cryptobyte.Builder) {
		b.AddASN1BigInt(r)
		b.AddASN1BigInt(s)
	})

	return b.Bytes()
}

// VerifySignature verifies the signature for the given message. Unless provided
// in an option, the digest of the message will be computed using the hash function specified
// when the SignerVerifier was created.
//
// This function returns nil if the verification succeeded, and an error message otherwise.
//
// This function recognizes the following Options listed in order of preference:
//
// - WithDigest()
//
// All other options are ignored if specified.
func (a *SignerVerifier) VerifySignature(sig, message io.Reader, opts ...signature.VerifyOption) error {
	ctx := context.Background()
	var digest []byte
	var signerOpts crypto.SignerOpts = a.hashFunc
	for _, opt := range opts {
		opt.ApplyDigest(&digest)
	}

	digest, _, err := signature.ComputeDigestForVerifying(message, signerOpts.HashFunc(), azureSupportedHashFuncs, opts...)
	if err != nil {
		return err
	}

	sigBytes, err := io.ReadAll(sig)
	if err != nil {
		return errors.Wrap(err, "reading signature")
	}

	// Convert the ANS.1 Sequence to a concantenated r||s byte string
	// This logic is borrowed from https://cs.opensource.google/go/go/+/refs/tags/go1.17.3:src/crypto/ecdsa/ecdsa.go;l=339
	var (
		r, s  = &big.Int{}, &big.Int{}
		inner cryptobyte.String
	)
	input := cryptobyte.String(sigBytes)
	if !input.ReadASN1(&inner, asn1.SEQUENCE) ||
		!input.Empty() ||
		!inner.ReadASN1Integer(r) ||
		!inner.ReadASN1Integer(s) ||
		!inner.Empty() {
		return errors.New("parsing signature")
	}

	rawSigBytes := []byte{}
	rawSigBytes = append(rawSigBytes, r.Bytes()...)
	rawSigBytes = append(rawSigBytes, s.Bytes()...)
	return a.client.verify(ctx, rawSigBytes, digest)
}

// PublicKey returns the public key that can be used to verify signatures created by
// this signer. All options provided in arguments to this method are ignored.
func (a *SignerVerifier) PublicKey(_ ...signature.PublicKeyOption) (crypto.PublicKey, error) {
	return a.client.public()
}

// CreateKey attempts to create a new key in Vault with the specified algorithm.
func (a *SignerVerifier) CreateKey(ctx context.Context, algorithm string) (crypto.PublicKey, error) {
	return a.client.createKey(ctx)
}

type cryptoSignerWrapper struct {
	ctx      context.Context
	hashFunc crypto.Hash
	sv       *SignerVerifier
	errFunc  func(error)
}

func (c cryptoSignerWrapper) Public() crypto.PublicKey {
	pk, err := c.sv.PublicKey(options.WithContext(c.ctx))
	if err != nil && c.errFunc != nil {
		c.errFunc(err)
	}
	return pk
}

func (c cryptoSignerWrapper) Sign(_ io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	hashFunc := c.hashFunc
	if opts != nil {
		hashFunc = opts.HashFunc()
	}
	gcpOptions := []signature.SignOption{
		options.WithContext(c.ctx),
		options.WithDigest(digest),
		options.WithCryptoSignerOpts(hashFunc),
	}

	return c.sv.SignMessage(nil, gcpOptions...)
}

func (a *SignerVerifier) CryptoSigner(ctx context.Context, errFunc func(error)) (crypto.Signer, crypto.SignerOpts, error) {
	csw := &cryptoSignerWrapper{
		ctx:      ctx,
		sv:       a,
		hashFunc: a.hashFunc,
		errFunc:  errFunc,
	}

	return csw, a.hashFunc, nil
}

func (*SignerVerifier) SupportedAlgorithms() []string {
	return azureSupportedAlgorithms
}

func (*SignerVerifier) DefaultAlgorithm() string {
	return Algorithm_ES256
}
