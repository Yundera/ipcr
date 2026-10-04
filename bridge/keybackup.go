package main

// Publisher key backups. The IPNS key a publisher's name belongs to (e.g. the forge's `forge` key)
// exists only in Kubo's keystore: lose the disk and the name is gone for good. A backup is that
// key, encrypted with a passphrase, in a small JSON file:
//
//	{"v":1, "kdf":"pbkdf2-sha256", "iter":600000, "salt":…, "nonce":…,
//	 "name":"forge", "id":"k51…", "format":"libp2p-protobuf-cleartext", "ct":…}
//
// ct = AES-256-GCM(key = PBKDF2-SHA256(passphrase, salt, iter, 32), nonce, plaintext,
// additional data = "ipcrkey/v1|<name>|<id>"), the plaintext being the key exactly as Kubo stores
// and exports it (`ipfs key export`, libp2p-protobuf-cleartext). The additional data binds name and
// id to the ciphertext, so neither can be edited. Byte slices are base64 (encoding/json's default).
//
// A copy of gateway/keybackup.go (ipcrd), without its CLI: the bridge writes backups (it can read
// Kubo's keystore; ipcrd cannot) and ipcrd reads them, so the cleartext key never crosses the
// network. Keep the two identical; both test the same vector.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
)

type keyBackup struct {
	V      int    `json:"v"`
	KDF    string `json:"kdf"`
	Iter   int    `json:"iter"`
	Salt   []byte `json:"salt"`
	Nonce  []byte `json:"nonce"`
	Name   string `json:"name"`
	ID     string `json:"id"`
	Format string `json:"format"`
	CT     []byte `json:"ct"`
}

const (
	backupIter   = 600_000
	backupFormat = "libp2p-protobuf-cleartext"
)

func (b *keyBackup) aad() []byte { return []byte("ipcrkey/v1|" + b.Name + "|" + b.ID) }

func backupCipher(pass string, salt []byte, iter int) (cipher.AEAD, error) {
	k, err := pbkdf2.Key(sha256.New, pass, salt, iter, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sealKey encrypts the Kubo key `key` (libp2p protobuf), named `name` in the keystore.
func sealKey(name string, key []byte, pass string) (*keyBackup, error) {
	if len(pass) < 12 {
		return nil, errors.New("passphrase: at least 12 characters")
	}
	id, err := ipnsName(key)
	if err != nil {
		return nil, err
	}
	b := &keyBackup{V: 1, KDF: "pbkdf2-sha256", Iter: backupIter, Name: name, ID: id, Format: backupFormat,
		Salt: make([]byte, 16), Nonce: make([]byte, 12)}
	rand.Read(b.Salt)
	rand.Read(b.Nonce)
	gcm, err := backupCipher(pass, b.Salt, b.Iter)
	if err != nil {
		return nil, err
	}
	b.CT = gcm.Seal(nil, b.Nonce, key, b.aad())
	return b, nil
}

// openKey decrypts a backup and checks that the key is the one its id names.
func openKey(b *keyBackup, pass string) ([]byte, error) {
	if b.V != 1 || b.KDF != "pbkdf2-sha256" || b.Format != backupFormat {
		return nil, fmt.Errorf("not an IPCR key backup (v%d, %s, %s)", b.V, b.KDF, b.Format)
	}
	if b.Iter < 100_000 || b.Iter > 10_000_000 || len(b.Salt) < 16 || len(b.Nonce) != 12 {
		return nil, errors.New("key backup: bad parameters")
	}
	gcm, err := backupCipher(pass, b.Salt, b.Iter)
	if err != nil {
		return nil, err
	}
	key, err := gcm.Open(nil, b.Nonce, b.CT, b.aad())
	if err != nil {
		return nil, errors.New("key backup: wrong passphrase, or the file was altered")
	}
	id, err := ipnsName(key)
	if err != nil {
		return nil, err
	}
	if id != b.ID {
		return nil, fmt.Errorf("key backup: the key is %s, the file says %s", id, b.ID)
	}
	return key, nil
}

// ipnsName returns the IPNS name (k51…) of an ed25519 key in libp2p's private-key protobuf:
// field 1 (type, 1 = Ed25519), field 2 (seed ‖ public key; 64 bytes, or 96 in keys from old
// libp2p versions, which repeat the public key). The name is the base36 CIDv1 (codec libp2p-key)
// of the identity multihash of the public key's protobuf — what Kubo prints with
// `--ipns-base base36`.
func ipnsName(priv []byte) (string, error) {
	if len(priv) < 4 || priv[0] != 0x08 || priv[1] != 0x01 || priv[2] != 0x12 || int(priv[3]) != len(priv)-4 {
		return "", errors.New("not an ed25519 libp2p private key (only ed25519 IPNS keys are supported)")
	}
	data := priv[4:]
	if len(data) != 64 && len(data) != 96 {
		return "", fmt.Errorf("ed25519 key: %d bytes", len(data))
	}
	seed, pub := data[:32], data[32:64]
	if !bytes.Equal(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey), pub) {
		return "", errors.New("ed25519 key: public half does not match")
	}
	pubProto := append([]byte{0x08, 0x01, 0x12, 0x20}, pub...)
	cid := append([]byte{0x01, 0x72, 0x00, byte(len(pubProto))}, pubProto...)
	return "k" + new(big.Int).SetBytes(cid).Text(36), nil
}
