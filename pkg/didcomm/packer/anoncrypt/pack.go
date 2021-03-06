/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package anoncrypt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/tink/go/keyset"

	"github.com/hyperledger/aries-framework-go/pkg/common/log"
	"github.com/hyperledger/aries-framework-go/pkg/crypto/tinkcrypto/primitive/composite"
	"github.com/hyperledger/aries-framework-go/pkg/crypto/tinkcrypto/primitive/composite/keyio"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/common/transport"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/packer"
	"github.com/hyperledger/aries-framework-go/pkg/doc/jose"
	"github.com/hyperledger/aries-framework-go/pkg/kms"
	"github.com/hyperledger/aries-framework-go/pkg/storage"
)

// Package anoncrypt includes a Packer implementation to build and parse JWE messages using Anoncrypt. It allows sending
// messages anonymously between parties with message repudiation, ie the sender identity is not revealed (and therefore
// not authenticated) to the recipient(s).

const encodingType = "didcomm-envelope-enc"

var logger = log.New("aries-framework/pkg/didcomm/packer/anoncrypt")

// Packer represents an Anoncrypt Pack/Unpacker that outputs/reads Aries envelopes
type Packer struct {
	kms    kms.KeyManager
	encAlg jose.EncAlg
}

// New will create an Packer instance to 'AnonCrypt' payloads for a given list of recipients.
// The returned Packer contains all the information required to pack and unpack payloads.
func New(ctx packer.Provider, encAlg jose.EncAlg) *Packer {
	k := ctx.KMS()

	return &Packer{
		kms:    k,
		encAlg: encAlg,
	}
}

// Pack will encode the payload argument
// Using the protocol defined by the Anoncrypt message of Aries RFC 0334
// Anoncrypt ignores the sender argument, it's added to meet the Packer interface
func (p *Packer) Pack(payload, _ []byte, recipientsPubKeys [][]byte) ([]byte, error) {
	if len(recipientsPubKeys) == 0 {
		return nil, fmt.Errorf("anoncrypt Pack: empty recipientsPubKeys")
	}

	recECKeys, err := unmarshalRecipientKeys(recipientsPubKeys)
	if err != nil {
		return nil, fmt.Errorf("anoncrypt Pack: failed to convert recipient keys: %w", err)
	}

	jweEncrypter, err := jose.NewJWEEncrypt(p.encAlg, recECKeys)
	if err != nil {
		return nil, fmt.Errorf("anoncrypt Pack: failed to new JWEEncrypt instance: %w", err)
	}

	jwe, err := jweEncrypter.Encrypt(payload)
	if err != nil {
		return nil, fmt.Errorf("anoncrypt Pack: failed to encrypt payload: %w", err)
	}

	var s string

	if len(recipientsPubKeys) == 1 {
		s, err = jwe.CompactSerialize(json.Marshal)
	} else {
		s, err = jwe.FullSerialize(json.Marshal)
	}

	if err != nil {
		return nil, fmt.Errorf("anoncrypt Pack: failed to serialize JWE message: %w", err)
	}

	return []byte(s), nil
}

func unmarshalRecipientKeys(keys [][]byte) ([]composite.PublicKey, error) {
	var pubKeys []composite.PublicKey

	for _, key := range keys {
		var ecKey composite.PublicKey

		err := json.Unmarshal(key, &ecKey)
		if err != nil {
			return nil, err
		}

		pubKeys = append(pubKeys, ecKey)
	}

	return pubKeys, nil
}

// Unpack will decode the envelope using a standard format
func (p *Packer) Unpack(envelope []byte) (*transport.Envelope, error) {
	jwe, err := jose.Deserialize(string(envelope))
	if err != nil {
		return nil, fmt.Errorf("anoncrypt Unpack: failed to deserialize JWE message: %w", err)
	}

	for i := range jwe.Recipients {
		kid, err := getKID(i, jwe)
		if err != nil {
			return nil, fmt.Errorf("anoncrypt Unpack: %w", err)
		}

		kh, err := p.kms.Get(kid)
		if err != nil {
			if strings.EqualFold(err.Error(), fmt.Sprintf("cannot read data for keysetID %s: %s", kid,
				storage.ErrDataNotFound)) {
				retriesMsg := ""

				if i < len(jwe.Recipients) {
					retriesMsg = ", will try another recipient"
				}

				logger.Debugf("anoncrypt Unpack: recipient keyID not found in KMS: %v%s", kid, retriesMsg)

				continue
			}

			return nil, fmt.Errorf("anoncrypt Unpack: failed to get key from kms: %w", err)
		}

		keyHandle, ok := kh.(*keyset.Handle)
		if !ok {
			return nil, fmt.Errorf("anoncrypt Unpack: invalid keyset handle")
		}

		jweDecrypter := jose.NewJWEDecrypt(keyHandle)

		pt, err := jweDecrypter.Decrypt(jwe)
		if err != nil {
			return nil, fmt.Errorf("anoncrypt Unpack: failed to decrypt JWE envelope: %w", err)
		}

		// TODO get mapped verKey for the recipient encryption key (kid)
		ecdhesPubKeyByes, err := exportPubKeyBytes(keyHandle)
		if err != nil {
			return nil, fmt.Errorf("anoncrypt Unpack: failed to export public key bytes: %w", err)
		}

		return &transport.Envelope{
			Message:  pt,
			ToVerKey: ecdhesPubKeyByes,
		}, nil
	}

	return nil, fmt.Errorf("anoncrypt Unpack: no matching recipient in envelope")
}

func getKID(i int, jwe *jose.JSONWebEncryption) (string, error) {
	var kid string

	if i == 0 && len(jwe.Recipients) == 1 { // compact serialization, recipient headers are in jwe.ProtectedHeaders
		ok := false

		kid, ok = jwe.ProtectedHeaders.KeyID()
		if !ok {
			return "", fmt.Errorf("single recipient missing 'KID' in jwe.ProtectHeaders")
		}
	} else {
		kid = jwe.Recipients[i].Header.KID
	}

	return kid, nil
}

func exportPubKeyBytes(keyHandle *keyset.Handle) ([]byte, error) {
	pubKH, err := keyHandle.Public()
	if err != nil {
		return nil, err
	}

	buf := new(bytes.Buffer)
	pubKeyWriter := keyio.NewWriter(buf)

	err = pubKH.WriteWithNoSecrets(pubKeyWriter)
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// EncodingType for didcomm
func (p *Packer) EncodingType() string {
	return encodingType
}
