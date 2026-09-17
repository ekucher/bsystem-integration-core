// Package oidc validates authentik's tokens locally.
//
// Before this, every authenticated request made a server-to-server call to
// authentik's UserInfo endpoint. That is correct and it makes the identity
// provider a synchronous dependency of every request the platform serves: an
// authentik that is slow makes the whole platform slow, and an authentik that
// is down makes every token look invalid.
//
// A signed token does not need that. The signature is checked against a key
// the platform already holds, so validity is decided locally and authentik
// stays what it is — the issuer and the source of identity — without being in
// the path of every read.
//
// The package deliberately implements JWS verification rather than taking a
// dependency: it needs three of the algorithms and a key set, and the code it
// would import is far larger than the code it would replace. What it must not
// get wrong is the set of things it refuses, so that is where the tests are.
package oidc

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
)

// A key set as the provider publishes it.
type jwks struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// publicKey converts one JWK into a key this package can verify with.
//
// A key whose type is not one of these is skipped rather than refused: a
// provider may publish an encryption key beside its signing keys, and refusing
// the whole set because of one would be an outage caused by something
// harmless.
func (k jwk) publicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		n, err := decodeBigInt(k.N)
		if err != nil {
			return nil, fmt.Errorf("modulus: %w", err)
		}
		exponent, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, fmt.Errorf("exponent: %w", err)
		}
		if len(exponent) == 0 || len(exponent) > 8 {
			return nil, errors.New("exponent is not a usable size")
		}
		padded := make([]byte, 8)
		copy(padded[8-len(exponent):], exponent)
		e := binary.BigEndian.Uint64(padded)
		if e > 1<<31 {
			return nil, errors.New("exponent is out of range")
		}
		// A short modulus is a weak key, and accepting one would let a
		// provider that had been talked into publishing a 512-bit key sign
		// anything the platform then believed.
		if n.BitLen() < 2048 {
			return nil, fmt.Errorf("RSA key is %d bits, below the 2048-bit minimum", n.BitLen())
		}
		return &rsa.PublicKey{N: n, E: int(e)}, nil
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, fmt.Errorf("unsupported curve %q", k.Crv)
		}
		size := (curve.Params().BitSize + 7) / 8
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil {
			return nil, fmt.Errorf("x: %w", err)
		}
		y, err := base64.RawURLEncoding.DecodeString(k.Y)
		if err != nil {
			return nil, fmt.Errorf("y: %w", err)
		}
		// JWK coordinates are fixed-width for the curve. A short one is not
		// something to left-pad and hope about: it means the key does not
		// describe the curve it claims to.
		if len(x) != size || len(y) != size {
			return nil, errors.New("coordinates are not the curve's width")
		}
		// The uncompressed point form, which the parser checks is on the
		// curve. Building the key from its coordinates directly skips that
		// check and is deprecated for exactly that reason.
		point := make([]byte, 1+2*size)
		point[0] = 4
		copy(point[1:], x)
		copy(point[1+size:], y)
		key, err := ecdsa.ParseUncompressedPublicKey(curve, point)
		if err != nil {
			return nil, fmt.Errorf("point: %w", err)
		}
		return key, nil
	default:
		return nil, fmt.Errorf("unsupported key type %q", k.Kty)
	}
}

func decodeBigInt(encoded string) (*big.Int, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, errors.New("empty value")
	}
	return new(big.Int).SetBytes(raw), nil
}

func parseJWKS(body []byte) (map[string]crypto.PublicKey, error) {
	var set jwks
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("decode JWKS: %w", err)
	}
	keys := map[string]crypto.PublicKey{}
	for _, entry := range set.Keys {
		// "enc" is an encryption key. Using one to check a signature is a
		// category error, not a fallback.
		if entry.Use == "enc" {
			continue
		}
		key, err := entry.publicKey()
		if err != nil {
			continue
		}
		if entry.Kid == "" {
			// A set with one unnamed key is legal. It is stored under the
			// empty string so a token with no kid can still find it.
			keys[""] = key
			continue
		}
		keys[entry.Kid] = key
	}
	if len(keys) == 0 {
		return nil, errors.New("the key set contains no usable signing key")
	}
	return keys, nil
}

// verifySignature checks one signature with one key.
//
// The algorithm decides both the hash and the shape of the signature, and the
// key has to match it. An RSA key presented for an ES256 token, or the
// reverse, is refused here rather than somewhere further in.
func verifySignature(alg string, key crypto.PublicKey, signed, signature []byte) error {
	switch alg {
	case "RS256", "RS384", "RS512":
		rsaKey, ok := key.(*rsa.PublicKey)
		if !ok {
			return errors.New("the signing key is not an RSA key")
		}
		hash, digest := digestFor(alg, signed)
		return rsa.VerifyPKCS1v15(rsaKey, hash, digest, signature)
	case "PS256", "PS384", "PS512":
		rsaKey, ok := key.(*rsa.PublicKey)
		if !ok {
			return errors.New("the signing key is not an RSA key")
		}
		hash, digest := digestFor(alg, signed)
		return rsa.VerifyPSS(rsaKey, hash, digest, signature, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthAuto, Hash: hash})
	case "ES256", "ES384", "ES512":
		ecKey, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return errors.New("the signing key is not an ECDSA key")
		}
		hash, digest := digestFor(alg, signed)
		_ = hash
		// JWS uses the fixed-width r||s encoding rather than ASN.1, and the
		// halves are left-padded to the curve's size.
		size := (ecKey.Curve.Params().BitSize + 7) / 8
		if len(signature) != 2*size {
			return errors.New("signature is the wrong length for this curve")
		}
		r := new(big.Int).SetBytes(signature[:size])
		s := new(big.Int).SetBytes(signature[size:])
		if !ecdsa.Verify(ecKey, digest, r, s) {
			return errors.New("signature does not verify")
		}
		return nil
	default:
		// Includes "none", every HMAC algorithm, and anything a token invents.
		// HMAC is the one worth naming: a token signed HS256 with the
		// provider's *public* key verifies, if a verifier is careless enough
		// to treat the key as a shared secret. It is refused by never
		// reaching a code path that could.
		return fmt.Errorf("unsupported signing algorithm %q", alg)
	}
}

func digestFor(alg string, signed []byte) (crypto.Hash, []byte) {
	switch alg[2:] {
	case "384":
		sum := sha512.Sum384(signed)
		return crypto.SHA384, sum[:]
	case "512":
		sum := sha512.Sum512(signed)
		return crypto.SHA512, sum[:]
	default:
		sum := sha256.Sum256(signed)
		return crypto.SHA256, sum[:]
	}
}
