package api

import (
	"context"
	"strconv"
	"strings"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/dns"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// Ask says what this server answers for a question, by asking the snapshot
// the query path serves with the function the query path uses (D49).
func (s *Server) Ask(_ context.Context, req gen.AskRequestObject) (gen.AskResponseObject, error) {
	name, err := parseName("the name", req.Params.Name)
	if err != nil {
		return nil, err
	}
	typ := zone.TypeA
	if req.Params.Type != nil && *req.Params.Type != "" {
		if typ, err = zone.ParseRRType(*req.Params.Type); err != nil {
			return nil, notAType(*req.Params.Type)
		}
	}

	snap := s.snapshots.Snapshot()
	var a dns.Answer
	snap.Resolve(dns.Question{Name: name, Class: zone.ClassIN, Type: typ}, &a)

	out := gen.Asked{
		Name:          name.String(),
		Type:          typ.String(),
		Rcode:         rcodeName(a.Rcode),
		Authoritative: a.Authoritative,
		Outcome:       outcomeOf(&a, typ),
		Answer:        answered(a.Answer),
		Authority:     answered(a.Authority),
		Additional:    answered(a.Additional),
	}
	if apex, ok := snap.Covering(name); ok {
		out.Zone = ptr(apex.String())
	}
	if a.Extended.Present && a.Extended.Text != "" {
		out.Explanation = ptr(a.Extended.Text)
	}
	return gen.Ask200JSONResponse(out), nil
}

// outcomeOf names the kind of answer, by the response code, the AA bit and
// the sections, as RFC 1034 §4.3.2 and RFC 2308 tell them apart.
func outcomeOf(a *dns.Answer, asked zone.RRType) gen.AskedOutcome {
	switch {
	case a.Rcode == wire.RcodeRefused:
		return gen.AskedOutcomeRefused
	case a.Rcode == wire.RcodeNotImplemented:
		return gen.AskedOutcomeNotImplemented
	case a.Rcode == wire.RcodeNameError:
		return gen.AskedOutcomeNxdomain
	case a.Rcode != wire.RcodeSuccess:
		return gen.AskedOutcomeFailed
	case len(a.Answer) > 0 && a.Answer[0].Header().Rrtype == wire.TypeCNAME && asked != zone.TypeCNAME:
		return gen.AskedOutcomeAlias
	case len(a.Answer) > 0:
		return gen.AskedOutcomeAnswer
	case !a.Authoritative:
		return gen.AskedOutcomeReferral
	}
	return gen.AskedOutcomeNodata
}

// rcodeName is a response code as dig writes it.
func rcodeName(rcode int) string {
	if name, ok := wire.RcodeToString[rcode]; ok {
		return name
	}
	return "RCODE" + strconv.Itoa(rcode)
}

// answered writes the records of one section the way a zonefile would.
func answered(rrs []wire.RR) []gen.AnsweredRecord {
	out := make([]gen.AnsweredRecord, 0, len(rrs))
	for _, rr := range rrs {
		h := rr.Header()
		out = append(out, gen.AnsweredRecord{
			Name:  h.Name,
			Ttl:   int64(h.Ttl),
			Class: wire.Class(h.Class).String(),
			Type:  wire.Type(h.Rrtype).String(),
			Data:  strings.TrimPrefix(rr.String(), h.String()),
		})
	}
	return out
}
