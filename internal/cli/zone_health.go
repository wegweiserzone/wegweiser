package cli

import (
	"context"
	"fmt"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
)

// lameNS is a name server this zone points at and does not have an address for.
type lameNS struct {
	// Owner is the name the NS record sits on: the apex for this zone's own
	// name servers, a child name for a delegation.
	Owner string `json:"owner"`
	// Target is the name server that has no address here.
	Target string `json:"target"`
}

// lameNameServers finds the NS targets inside this zone that no A or AAAA
// answers for.
//
// A resolver that follows a delegation to a name server *inside* the zone it
// delegates to has a circular dependency, and the only way out of it is an
// address record served alongside: glue. Without one this server answers
// NXDOMAIN, authoritatively, for its own name server: the delegation is lame
// (RFC 1912 §2.8).
func lameNameServers(
	ctx context.Context, client *gen.ClientWithResponses, f *clientFlags, z *gen.Zone,
) ([]lameNS, error) {
	nsType := "NS"
	page, err := client.ListRecordsWithResponse(ctx, z.Id,
		&gen.ListRecordsParams{Type: &nsType, Limit: ptr(store1000)})
	if err != nil {
		return nil, reachable(err, f.server)
	}
	if page.JSON200 == nil {
		return nil, apiError(page.HTTPResponse.StatusCode, page.Body)
	}

	// One lookup per distinct target, not per record: two name servers on one
	// name is one question.
	seen := make(map[string]bool)
	var lame []lameNS

	// Indexed rather than ranged by value: a record is a large struct and this
	// only reads two fields of it.
	for i := range page.JSON200.Items {
		ns := &page.JSON200.Items[i]
		if !inside(ns.Data, z.Name) || seen[ns.Data] {
			continue
		}
		seen[ns.Data] = true

		addressed, aerr := hasAddress(ctx, client, f, z.Id, ns.Data)
		if aerr != nil {
			return nil, aerr
		}
		if !addressed {
			lame = append(lame, lameNS{Owner: ns.Name, Target: ns.Data})
		}
	}
	return lame, nil
}

// store1000 is one page large enough that no zone's name servers spill over it.
const store1000 = 1000

// inside reports whether name is at or below apex.
func inside(name, apex string) bool {
	return name == apex || (len(name) > len(apex) && name[len(name)-len(apex)-1:] == "."+apex)
}

// hasAddress reports whether the zone answers for this name with an address.
func hasAddress(
	ctx context.Context, client *gen.ClientWithResponses, f *clientFlags,
	zoneID, name string,
) (bool, error) {
	for _, typ := range []string{"A", "AAAA"} {
		page, err := client.ListRecordsWithResponse(ctx, zoneID,
			&gen.ListRecordsParams{Name: &name, Type: &typ, Limit: ptr(1)})
		if err != nil {
			return false, reachable(err, f.server)
		}
		if page.JSON200 == nil {
			return false, apiError(page.HTTPResponse.StatusCode, page.Body)
		}
		if len(page.JSON200.Items) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// lameNote is the sentence a client prints about one lame delegation.
func lameNote(l lameNS) string {
	return fmt.Sprintf(
		"%s has no address in this zone, so a resolver referred to it is told the name does "+
			"not exist. Add %s A <address>, or point the delegation somewhere off-site.",
		l.Target, l.Target)
}
