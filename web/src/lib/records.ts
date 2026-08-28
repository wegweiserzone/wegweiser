/**
 * The record types this interface suggests.
 *
 * Not a closed list: the server stores anything with a mnemonic, and anything
 * without one in the `TYPE<number>` form of RFC 3597 §5, so the field these
 * feed accepts free text. What this is, is the order somebody actually reaches
 * for them in.
 */

/** TypeGroup is a set of record types with what they are for. */
export interface TypeGroup {
  label: string;
  types: string[];
}

/** suggestedTypes are what the type field offers, in the order it offers them. */
export const suggestedTypes: TypeGroup[] = [
  { label: "Addresses", types: ["A", "AAAA"] },
  { label: "Names", types: ["CNAME", "NS", "PTR", "DNAME"] },
  { label: "Mail", types: ["MX", "TXT", "SPF"] },
  { label: "Services", types: ["SRV", "SVCB", "HTTPS", "NAPTR", "URI"] },
  { label: "Security", types: ["CAA", "TLSA", "SSHFP"] },
  { label: "Other", types: ["LOC", "HINFO", "RP"] },
];

/** everyType is the flat list, for anything that does not want the grouping. */
export const everyType: string[] = suggestedTypes.flatMap((g) => g.types);
