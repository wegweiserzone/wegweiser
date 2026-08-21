/**
 * What a record's data is made of, type by type.
 */

/** Part is one component of a record's data. */
export interface Part {
  label: string;
  hint?: string;
  placeholder?: string;
  /** Numeric parts get a numeric keyboard and a narrower column. */
  number?: boolean;
  /**
   * A character-string of RFC 1035 §3.3.14, which is quoted on the wire. The
   * form takes it unquoted, because quoting is exactly the syntax nobody
   * should have to think about.
   */
  quoted?: boolean;
  /** Suggestions, when the part has a small set of usual values. */
  options?: string[];
}

/** Shape is how one record type's data is put together. */
export interface Shape {
  parts: Part[];
  /** What the assembled line looks like, shown under the form. */
  example: string;
}

const name = (label: string, hint: string): Part => ({
  label,
  hint,
  placeholder: "example.com.",
});

/**
 * shapes covers the types somebody types by hand. Anything absent falls back
 * to a single box, including every type this list has never heard of, which
 * is the case that must keep working.
 */
export const shapes: Record<string, Shape> = {
  A: {
    parts: [{ label: "Address", hint: "An IPv4 address.", placeholder: "192.0.2.10" }],
    example: "192.0.2.10",
  },
  AAAA: {
    parts: [{ label: "Address", hint: "An IPv6 address.", placeholder: "2001:db8::10" }],
    example: "2001:db8::10",
  },
  CNAME: {
    parts: [name("Points at", "The name this one is an alias for.")],
    example: "example.com.",
  },
  DNAME: {
    parts: [name("Points at", "The subtree this one stands in for.")],
    example: "example.com.",
  },
  NS: {
    parts: [name("Name server", "A server authoritative for this name.")],
    example: "ns1.example.com.",
  },
  PTR: {
    parts: [name("Points at", "The name this address belongs to.")],
    example: "host.example.com.",
  },
  MX: {
    parts: [
      {
        label: "Preference",
        hint: "Lower is tried first.",
        placeholder: "10",
        number: true,
      },
      name("Mail server", "Where mail for this name goes. Never a CNAME (RFC 2181 §10.3)."),
    ],
    example: "10 mail.example.com.",
  },
  SRV: {
    parts: [
      { label: "Priority", hint: "Lower is tried first.", placeholder: "10", number: true },
      {
        label: "Weight",
        hint: "Shares traffic between equal priorities.",
        placeholder: "60",
        number: true,
      },
      { label: "Port", hint: "The port on the target.", placeholder: "5060", number: true },
      name("Target", "The host running the service. Never a CNAME."),
    ],
    example: "10 60 5060 pbx.example.com.",
  },
  TXT: {
    parts: [
      {
        label: "Text",
        hint: "Written as you mean it; the quoting is added.",
        placeholder: "v=spf1 mx -all",
        quoted: true,
      },
    ],
    example: '"v=spf1 mx -all"',
  },
  SPF: {
    parts: [
      {
        label: "Text",
        hint: "Written as you mean it; the quoting is added.",
        placeholder: "v=spf1 mx -all",
        quoted: true,
      },
    ],
    example: '"v=spf1 mx -all"',
  },
  CAA: {
    parts: [
      { label: "Flags", hint: "128 makes it critical, 0 otherwise.", placeholder: "0", number: true },
      {
        label: "Tag",
        hint: "What this says about certificates.",
        placeholder: "issue",
        options: ["issue", "issuewild", "iodef"],
      },
      {
        label: "Value",
        hint: "The authority, or a contact for iodef.",
        placeholder: "letsencrypt.org",
        quoted: true,
      },
    ],
    example: '0 issue "letsencrypt.org"',
  },
  SSHFP: {
    parts: [
      { label: "Algorithm", hint: "1 RSA, 2 DSA, 3 ECDSA, 4 Ed25519.", placeholder: "4", number: true },
      { label: "Type", hint: "1 SHA-1, 2 SHA-256.", placeholder: "2", number: true },
      { label: "Fingerprint", hint: "In hexadecimal.", placeholder: "abcdef0123…" },
    ],
    example: "4 2 abcdef0123…",
  },
  TLSA: {
    parts: [
      { label: "Usage", hint: "0–3, as RFC 6698 §2.1.1 numbers them.", placeholder: "3", number: true },
      { label: "Selector", hint: "0 full certificate, 1 public key.", placeholder: "1", number: true },
      { label: "Matching", hint: "0 exact, 1 SHA-256, 2 SHA-512.", placeholder: "1", number: true },
      { label: "Data", hint: "In hexadecimal.", placeholder: "abcdef0123…" },
    ],
    example: "3 1 1 abcdef0123…",
  },
  URI: {
    parts: [
      { label: "Priority", hint: "Lower is tried first.", placeholder: "10", number: true },
      { label: "Weight", hint: "Shares traffic between equal priorities.", placeholder: "1", number: true },
      { label: "Target", hint: "The URI itself.", placeholder: "https://example.com/", quoted: true },
    ],
    example: '10 1 "https://example.com/"',
  },
  HINFO: {
    parts: [
      { label: "CPU", hint: "The hardware, as a word.", placeholder: "x86_64", quoted: true },
      { label: "OS", hint: "The operating system, as a word.", placeholder: "linux", quoted: true },
    ],
    example: '"x86_64" "linux"',
  },
  RP: {
    parts: [
      name("Mailbox", "The responsible person, as a domain name."),
      name("Text record", "A TXT record with more about them, or . for none."),
    ],
    example: "hostmaster.example.com. .",
  },
  SVCB: {
    parts: [
      { label: "Priority", hint: "0 is an alias form; 1 and up are service forms.", placeholder: "1", number: true },
      name("Target", "The host the service is on, or . for this name."),
      { label: "Parameters", hint: "Key=value pairs, space separated.", placeholder: 'alpn="h2,h3"' },
    ],
    example: '1 . alpn="h2,h3"',
  },
  HTTPS: {
    parts: [
      { label: "Priority", hint: "0 is an alias form; 1 and up are service forms.", placeholder: "1", number: true },
      name("Target", "The host the service is on, or . for this name."),
      { label: "Parameters", hint: "Key=value pairs, space separated.", placeholder: 'alpn="h2,h3"' },
    ],
    example: '1 . alpn="h2,h3"',
  },
};

/** shapeOf returns the form for a type, or undefined for one box. */
export function shapeOf(type: string): Shape | undefined {
  return shapes[type.toUpperCase()];
}

/**
 * quote wraps a character-string the way the presentation format wants it.
 */
function quote(value: string): string {
  const text = value.trim();
  if (text.startsWith('"') && text.endsWith('"') && text.length >= 2) return text;
  return `"${text.replace(/\\/g, "\\\\").replace(/"/g, '\\"')}"`;
}

/** assemble turns the parts back into one line of presentation format. */
export function assemble(shape: Shape, values: string[]): string {
  return shape.parts
    .map((part, i) => {
      const value = (values[i] ?? "").trim();
      if (!value) return "";
      return part.quoted ? quote(value) : value;
    })
    .filter((piece) => piece !== "")
    .join(" ");
}

/**
 * disassemble splits a line back into the parts, for editing.
 */
export function disassemble(shape: Shape, data: string): string[] {
  const values: string[] = [];
  let rest = data.trim();

  shape.parts.forEach((part, i) => {
    const last = i === shape.parts.length - 1;
    if (last) {
      values.push(part.quoted ? unquote(rest) : rest);
      rest = "";
      return;
    }
    const space = rest.indexOf(" ");
    if (space < 0) {
      values.push(part.quoted ? unquote(rest) : rest);
      rest = "";
      return;
    }
    values.push(part.quoted ? unquote(rest.slice(0, space)) : rest.slice(0, space));
    rest = rest.slice(space + 1).trimStart();
  });

  return values;
}

/** unquote is quote's opposite, for showing a stored value in a field. */
function unquote(value: string): string {
  const text = value.trim();
  if (!text.startsWith('"') || !text.endsWith('"') || text.length < 2) return text;
  return text.slice(1, -1).replace(/\\"/g, '"').replace(/\\\\/g, "\\");
}
