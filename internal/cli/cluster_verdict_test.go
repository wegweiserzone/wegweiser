package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/cli/output"
)

// voters builds a status of voters, the first leading where led is set, the
// ones named in off not reached.
func voters(led bool, quorum *gen.ClusterQuorum, ids []string, off ...string) *gen.ClusterStatus {
	st := &gen.ClusterStatus{Replicating: true, Quorum: quorum, Self: gen.ClusterMember{Id: ids[0]}}
	for i, id := range ids {
		m := gen.ClusterMemberState{Id: id, Role: gen.ClusterMemberStateRoleVoter, Leader: led && i == 0}
		if !strings.Contains(strings.Join(off, " "), id) {
			m.Progress = &gen.ClusterProgress{}
		}
		st.Members = append(st.Members, m)
	}
	return st
}

func TestVerdict(t *testing.T) {
	t.Parallel()
	p := output.New(new(bytes.Buffer), new(bytes.Buffer), output.FormatText)
	three := []string{"ns1", "ns2", "ns3"}

	tests := []struct {
		name string
		st   *gen.ClusterStatus
		want string
	}{
		{"all there", voters(true, &gen.ClusterQuorum{Voters: 3, Needed: 2, Answered: 3}, three),
			"takes writes, and can lose one voter: 3 of 3 voters answered, 2 needed"},
		{"one off", voters(true, &gen.ClusterQuorum{Voters: 3, Needed: 2, Answered: 2}, three, "ns3"),
			"takes writes, and the next voter lost stops them: 2 of 3 voters answered, 2 needed"},
		{"two off", voters(false, &gen.ClusterQuorum{Voters: 3, Needed: 2, Answered: 1}, three, "ns2", "ns3"),
			"takes no writes: 1 of 3 voters answered, 2 needed"},
		{"between leaders", voters(false, &gen.ClusterQuorum{Voters: 3, Needed: 2, Answered: 3}, three),
			"takes no writes until a leader is elected: 3 of 3 voters answered, 2 needed"},
		{"five, all there", voters(true, &gen.ClusterQuorum{Voters: 5, Needed: 3, Answered: 5},
			[]string{"a", "b", "c", "d", "e"}),
			"takes writes, and can lose 2 voters: 5 of 5 voters answered, 3 needed"},
		{"not counted", voters(true, nil, three), ""},
	}
	for _, tt := range tests {
		if got := verdict(p, tt.st); got != tt.want {
			t.Errorf("%s: verdict = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// Before a member is taken out, what that leaves of the majority is said, and
// the case that stops writes is the one it is there for.
func TestWarnAboutMajority(t *testing.T) {
	t.Parallel()
	three := []string{"ns1", "ns2", "ns3"}
	oneOff := voters(true, &gen.ClusterQuorum{Voters: 3, Needed: 2, Answered: 2}, three, "ns3")

	tests := []struct {
		name string
		st   *gen.ClusterStatus
		id   string
		want string
	}{
		{"the one that is off", oneOff, "ns3",
			"Afterwards 2 voters remain, 2 of them answered here, and 2 are needed: writes go on, and the next voter lost stops them."},
		{"a running one while another is off", oneOff, "ns2",
			"Afterwards 2 voters remain, 1 of them answered here, and 2 are needed: the cluster would take no writes."},
		{"no majority to start with", voters(false, &gen.ClusterQuorum{Voters: 3, Needed: 2, Answered: 1},
			three, "ns2", "ns3"), "ns2", ""},
	}
	for _, tt := range tests {
		var errOut bytes.Buffer
		opts := &options{stdout: new(bytes.Buffer), stderr: &errOut, format: output.FormatText}
		warnAboutMajority(opts, tt.st, tt.id)
		if got := strings.TrimSpace(errOut.String()); got != tt.want {
			t.Errorf("%s: said %q, want %q", tt.name, got, tt.want)
		}
	}
}
