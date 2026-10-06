package cluster

import (
	"errors"
	"strings"

	"github.com/hashicorp/raft"
)

// WitnessPrefix begins the identifier of every witness, and of nothing else
// (docs/decisions/d48-a-witness-is-known-by-its-identifier.md). wegwitness
// holds the same constant and points at the same record.
const WitnessPrefix = "witness-"

// IsWitness reports whether the member by this identifier is a witness.
func IsWitness(id string) bool { return strings.HasPrefix(id, WitnessPrefix) }

// ErrWitnessMajority is a change after which witnesses would no longer be
// fewer than half of the voters (docs/decisions/d39-the-witness.md).
var ErrWitnessMajority = errors.New("cluster: witnesses have to stay fewer than half of the voters")

// keepsWitnessesFew checks the voters the cluster would have once the member
// by this identifier votes, or once it is gone when voting is false.
func (n *Node) keepsWitnessesFew(id string, voting bool) error {
	voters, witnesses := 0, 0
	for _, srv := range n.raft.GetConfiguration().Configuration().Servers {
		if string(srv.ID) == id || srv.Suffrage != raft.Voter {
			continue
		}
		voters++
		if IsWitness(string(srv.ID)) {
			witnesses++
		}
	}
	if voting {
		voters++
		if IsWitness(id) {
			witnesses++
		}
	}
	if witnesses > 0 && 2*witnesses >= voters {
		return ErrWitnessMajority
	}
	return nil
}
