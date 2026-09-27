package keys

import (
	"fmt"

	p2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Libp2pKey is the node key of m/7333'/0'/0' as a libp2p secp256k1 key.
func (s *Set) Libp2pKey() (p2pcrypto.PrivKey, error) {
	k, err := p2pcrypto.UnmarshalSecp256k1PrivateKey(s.Libp2p.Serialize())
	if err != nil {
		return nil, fmt.Errorf("libp2p key: %w", err)
	}
	return k, nil
}

// PeerID is the libp2p peer id of the node key (16Uiu2…).
func (s *Set) PeerID() (peer.ID, error) {
	k, err := s.Libp2pKey()
	if err != nil {
		return "", err
	}
	return peer.IDFromPrivateKey(k)
}
