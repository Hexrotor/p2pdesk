package nodekit

import (
	"crypto/rand"
	"os"
	"path/filepath"

	"github.com/libp2p/go-libp2p/core/crypto"
)

// IdentityFileName matches the rust-libp2p identity file (libp2p
// protobuf-encoded ed25519 private key). Go's UnmarshalPrivateKey reads it
// as-is, so a PeerId minted by the Rust implementation survives the
// migration unchanged.
const IdentityFileName = "identity.key"

// LoadIdentity loads the persistent identity from identityDir, generating
// and persisting a fresh ed25519 keypair when the file is missing or
// corrupt. The write is atomic (temp + rename) with 0600 permissions.
func LoadIdentity(identityDir string) (crypto.PrivKey, error) {
	path := filepath.Join(identityDir, IdentityFileName)
	data, err := os.ReadFile(path)
	if err == nil {
		priv, err2 := crypto.UnmarshalPrivateKey(data)
		if err2 == nil {
			return priv, nil
		}
		// corrupt file: fall through and regenerate
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, err
	}
	bytes, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(identityDir, 0o700); err != nil {
		return nil, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, bytes, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	return priv, nil
}
