package dns

import (
	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// zoneTree is one zone in the form the query path reads it.
//
// Both shapes were measured with the depth bound below applied. The tree wins
// on names far deeper than the zone; the map wins on every shape that occurs
// in traffic, including a random-subdomain flood.
type zoneTree struct {
	name zone.Name
	soa  zone.SOA

	// negTTL is the TTL every negative answer from this zone carries, which
	// RFC 2308 §3 and §5 make the lesser of the SOA record's TTL and the SOA
	// MINIMUM field.
	negTTL zone.TTL

	// negSOA is the SOA a negative answer puts in its authority section, built
	// once with negTTL already applied.
	negSOA wire.RR

	nodes map[zone.Name]*node

	// depth is how far the deepest name in the zone sits below the apex. A
	// longer query name cannot match, so it is cut back before the walk and the
	// work a query can ask for is bounded by the zone, not by the query.
	depth int

	// count is the number of records the zone answers from, including the
	// synthesised SOA.
	count int
}

// node is one owner name in a zone.
//
// A node exists for every name carrying records and every name in between, so
// an empty non-terminal is a node with no RRsets rather than a missing node.
// That distinction is NODATA against NXDOMAIN (RFC 4592 §2.2.2, RFC 8020).
type node struct {
	name zone.Name

	// sets are the RRsets at this name. A slice, because an owner name carries
	// a handful of types at most, and a linear scan over three entries costs
	// less than hashing a key.
	sets []rrset

	// wild is the "*" child of this node, the wildcard RFC 4592 §3.3.1
	// synthesises from when a query finds no closer name. Linked at build time,
	// so ruling one out costs a nil check rather than a lookup.
	wild *node

	// delegation is the node at or above this one whose NS records hand the
	// name to another server, and is nil for a name we answer for ourselves. A
	// delegation node points at itself, so one field answers both "is this a
	// delegation" and "is this below one".
	delegation *node
}

// rrset is the set of records sharing an owner name, class and type. DNS
// answers it as a unit (RFC 2181 §5).
type rrset struct {
	class zone.Class
	typ   zone.RRType
	rrs   []wire.RR

	// targets holds the name each record points at, in the same order as rrs,
	// nil for every type [pointsAtAName] does not admit. Converting the
	// presentation form to a [zone.Name] allocates, so it happens once per
	// rebuild rather than once per query (D12).
	targets []zone.Name
}

// find returns the RRset of the given class and type at this name, or nil.
func (n *node) find(c zone.Class, t zone.RRType) *rrset {
	for i := range n.sets {
		if n.sets[i].typ == t && n.sets[i].class == c {
			return &n.sets[i]
		}
	}
	return nil
}

// empty reports whether this name carries no records of its own, which makes it
// an empty non-terminal: it exists, and a query for it is NODATA rather than
// NXDOMAIN.
func (n *node) empty() bool { return len(n.sets) == 0 }

// add appends a record to its RRset, starting the set if this is the first of
// its kind at this name, and returns the set it landed in.
func (n *node) add(c zone.Class, t zone.RRType, rr wire.RR) *rrset {
	if set := n.find(c, t); set != nil {
		set.rrs = append(set.rrs, rr)
		return set
	}
	n.sets = append(n.sets, rrset{class: c, typ: t, rrs: []wire.RR{rr}})
	return &n.sets[len(n.sets)-1]
}

// lookup is what a walk towards a name found.
type lookup struct {
	// node is the node for the name exactly, and is nil when no such name
	// exists in the zone.
	node *node

	// closest is the deepest node at or above the name: the closest encloser
	// of RFC 4592 §3.3.1, which is where wildcard synthesis starts. It is the
	// same node as [lookup.node] for a name that exists, and is nil only when
	// the name lies outside the zone.
	closest *node

	// delegation is the node whose NS records take over from here, and is nil
	// while the zone still answers for itself. It is set whether the name is
	// the delegation point or lies below it, and a caller must act on it before
	// anything else: below a delegation this zone's data is not authoritative,
	// and the answer is a referral (RFC 1034 §4.3.2 step 3b).
	delegation *node
}

// lookup finds qname in the zone, and the closest encloser when it is not
// there.
func (t *zoneTree) lookup(qname zone.Name) lookup {
	name := qname
	limit := t.name.LabelCount() + t.depth
	for depth := name.LabelCount(); depth > limit; depth-- {
		parent, ok := name.Parent()
		if !ok {
			break
		}
		name = parent
	}

	for cur := name; ; {
		if n := t.nodes[cur]; n != nil {
			l := lookup{closest: n, delegation: n.delegation}
			if cur.Equal(qname) {
				l.node = n
			}
			return l
		}
		parent, ok := cur.Parent()
		if !ok {
			return lookup{} // the name lies outside this zone
		}
		cur = parent
	}
}

// node returns the node for name, creating it and every name between it and the
// apex if they are not there yet. The names in between are what make an empty
// non-terminal exist.
//
// The name must lie inside the zone; the builder checks that before calling.
func (t *zoneTree) node(name zone.Name) *node {
	if n, ok := t.nodes[name]; ok {
		return n
	}

	n := &node{name: name}
	t.nodes[name] = n
	if d := name.LabelCount() - t.name.LabelCount(); d > t.depth {
		t.depth = d
	}
	if !name.Equal(t.name) {
		if parent, ok := name.Parent(); ok {
			t.node(parent)
		}
	}
	return n
}

// link fills in the two shortcuts a lookup relies on: the wildcard child of a
// name and the delegation above it, both worked out once rather than per query.
func (t *zoneTree) link(delegations map[zone.Name]*node) {
	for name, n := range t.nodes {
		if label, ok := name.FirstLabel(); ok && label == "*" && !name.Equal(t.name) {
			if parent, ok := name.Parent(); ok {
				if p := t.nodes[parent]; p != nil {
					p.wild = n
				}
			}
		}

		if len(delegations) == 0 || n.delegation != nil {
			continue
		}
		for cur := name; !cur.Equal(t.name); {
			parent, ok := cur.Parent()
			if !ok {
				break
			}
			if d := delegations[parent]; d != nil {
				n.delegation = d
				break
			}
			cur = parent
		}
	}
}
