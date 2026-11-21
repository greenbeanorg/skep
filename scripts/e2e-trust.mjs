// Trust for the browser tests (ADR 0010 amendment x, option c): skep-api's
// certificate is trusted only when the SHA-256 of its public key
// (SubjectPublicKeyInfo) equals the pin `skep-api tls fingerprint` prints.
//
//   node scripts/e2e-trust.mjs check <cert.pem> <pin>
//       exit 0 if the certificate file carries the pinned key
//   node scripts/e2e-trust.mjs fetch <host:port> <pin> <out.pem>
//       read the certificate a running skep-api serves and write it to
//       out.pem only if it carries the pinned key; nothing is sent over
//       the connection, which is closed once the certificate is read
//
// The written file is what scripts/web-e2e.sh gives Node as
// NODE_EXTRA_CA_CERTS, so Playwright's request client verifies the server
// against it; Chromium is pinned with --ignore-certificate-errors-spki-list.
import { X509Certificate, createHash } from "node:crypto";
import fs from "node:fs";
import net from "node:net";
import tls from "node:tls";

function pinOf(cert) {
  const der = cert.publicKey.export({ type: "spki", format: "der" });
  return "sha256:" + createHash("sha256").update(der).digest("hex");
}

function fail(message) {
  console.error(`e2e-trust: ${message}`);
  process.exit(1);
}

function wantPin(pin) {
  if (!/^sha256:[0-9a-f]{64}$/.test(pin ?? "")) fail(`the pin must be sha256:<64 hex digits>, as skep-api tls fingerprint prints it; got ${JSON.stringify(pin)}`);
  return pin;
}

const [cmd, ...args] = process.argv.slice(2);
if (cmd === "check" && args.length === 2) {
  const pin = wantPin(args[1]);
  const got = pinOf(new X509Certificate(fs.readFileSync(args[0])));
  if (got !== pin) fail(`${args[0]} carries the key ${got}, not the pinned ${pin}`);
} else if (cmd === "fetch" && args.length === 3) {
  const pin = wantPin(args[1]);
  const at = args[0].lastIndexOf(":");
  if (at < 1) fail(`want host:port, got ${args[0]}`);
  const host = args[0].slice(0, at).replace(/^\[(.*)\]$/, "$1");
  const port = Number(args[0].slice(at + 1));
  // The certificate is not trusted yet: it is read, compared with the
  // pin, and only then trusted. Nothing is written to the connection.
  const socket = tls.connect({ host, port, rejectUnauthorized: false, servername: net.isIP(host) ? undefined : host });
  socket.setTimeout(10_000, () => fail(`no TLS handshake with ${args[0]} within 10 s`));
  socket.on("error", (err) => fail(`${args[0]}: ${err.message}`));
  socket.on("secureConnect", () => {
    const cert = socket.getPeerX509Certificate();
    socket.destroy();
    if (!cert) fail(`${args[0]} sent no certificate`);
    const got = pinOf(cert);
    if (got !== pin) fail(`${args[0]} serves the key ${got}, not the pinned ${pin}; refusing it`);
    fs.writeFileSync(args[2], cert.toString(), { mode: 0o600 });
  });
} else {
  fail("usage: e2e-trust.mjs check <cert.pem> <pin> | fetch <host:port> <pin> <out.pem>");
}
