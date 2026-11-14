import { FitAddon } from "@xterm/addon-fit";
import { WebglAddon } from "@xterm/addon-webgl";
import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { useEffect, useRef, useState } from "react";

import { api, consoleURL, reportActivity } from "@/api/client";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";

/** The close code skep-api uses when the session behind a console ended. */
const SESSION_ENDED = 4001;

type Status = "connecting" | "open" | "closed";

/**
 * The serial console of a VM in xterm.js (ADR 0009 decision 8). Bytes go
 * both ways unchanged; the terminal emulation happens here. Typing counts
 * as activity, as it does on the server; the guest's output does not.
 *
 * The terminal draws with WebGL: xterm's DOM renderer injects <style>
 * elements, which the interface's Content-Security-Policy (style-src
 * 'self') blocks, and the CSP is not loosened for it.
 */
export function SerialConsole({ vmId }: { vmId: string }) {
  const host = useRef<HTMLDivElement>(null);
  const [status, setStatus] = useState<Status>("connecting");
  const [reason, setReason] = useState("");
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    const el = host.current;
    if (!el) return;
    const term = new Terminal({
      cursorBlink: true,
      convertEol: false,
      scrollback: 5000,
      // The rows are also kept as text for assistive technology, so the
      // output is not only pixels on a canvas.
      screenReaderMode: true,
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace',
      fontSize: 13,
      theme: { background: "#0b0f14", foreground: "#d7dde5", cursor: "#d7dde5" },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    // Loaded before open(): the WebGL renderer is then in place when the
    // terminal opens, and xterm never creates its DOM renderer, whose
    // <style> elements the CSP would block. Without WebGL the DOM renderer
    // draws, without its colours and cell sizing.
    try {
      const webgl = new WebglAddon();
      webgl.onContextLoss(() => webgl.dispose());
      term.loadAddon(webgl);
    } catch {
      // No WebGL.
    }
    term.open(el);
    fit.fit();
    term.focus();

    const ws = new WebSocket(consoleURL(vmId));
    ws.binaryType = "arraybuffer";
    const encoder = new TextEncoder();
    ws.onopen = () => setStatus("open");
    ws.onmessage = (e: MessageEvent) => {
      if (e.data instanceof ArrayBuffer) term.write(new Uint8Array(e.data));
      else if (typeof e.data === "string") term.write(e.data);
    };
    ws.onclose = (e: CloseEvent) => {
      setStatus("closed");
      if (e.code === SESSION_ENDED) {
        setReason("Your session has ended.");
        // Ask the server, so the interface notices and shows the login.
        void api.session().catch(() => undefined);
      } else if (e.code === 1006) {
        setReason("The console could not be reached. The VM may have stopped, or four clients may be attached already.");
      } else {
        setReason(e.reason || "The console closed.");
      }
    };
    const send = (bytes: Uint8Array) => {
      if (ws.readyState !== WebSocket.OPEN) return;
      ws.send(bytes);
      reportActivity();
    };
    const onData = term.onData((d) => send(encoder.encode(d)));
    const onBinary = term.onBinary((d) => send(Uint8Array.from(d, (c) => c.charCodeAt(0) & 0xff)));
    const resize = new ResizeObserver(() => {
      try {
        fit.fit();
      } catch {
        // The element is being removed.
      }
    });
    resize.observe(el);

    return () => {
      resize.disconnect();
      onData.dispose();
      onBinary.dispose();
      ws.onclose = null;
      ws.close();
      term.dispose();
    };
  }, [vmId, attempt]);

  return (
    <div className="space-y-2">
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-2 text-xs text-muted-foreground">
          <Badge tone={status === "open" ? "ok" : status === "connecting" ? "info" : "neutral"}>
            {status === "open" ? "connected" : status}
          </Badge>
          {status === "closed" ? <span role="status">{reason}</span> : <span>Serial console (COM1). Click it to type.</span>}
        </div>
        {status === "closed" ? (
          <Button
            size="sm"
            variant="outline"
            onClick={() => {
              setStatus("connecting");
              setReason("");
              setAttempt((n) => n + 1);
            }}
          >
            Reconnect
          </Button>
        ) : null}
      </div>
      <div
        ref={host}
        data-testid="serial-console"
        className="h-[28rem] overflow-hidden rounded-md border bg-[#0b0f14] p-2"
        aria-label="Serial console"
      />
    </div>
  );
}
