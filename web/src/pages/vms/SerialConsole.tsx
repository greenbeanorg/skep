import { FitAddon } from "@xterm/addon-fit";
import { WebglAddon } from "@xterm/addon-webgl";
import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { useEffect, useRef, useState } from "react";

import { api, consoleURL, reportActivity } from "@/api/client";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Notice } from "@/components/ui/notice";

/** The close code skep-api uses when the session behind a console ended. */
const SESSION_ENDED = 4001;

type Status = "connecting" | "open" | "closed" | "unavailable";

/** Whether this browser can give the terminal a WebGL 2 context. */
export function webgl2Available(doc: Document = document): boolean {
  try {
    const canvas = doc.createElement("canvas");
    const gl = canvas.getContext("webgl2");
    if (!gl) return false;
    gl.getExtension("WEBGL_lose_context")?.loseContext();
    return true;
  } catch {
    return false;
  }
}

/**
 * The serial console of a VM in xterm.js (ADR 0009 decision 8). Bytes go
 * both ways unchanged; the terminal emulation happens here. Typing counts
 * as activity, as it does on the server; the guest's output does not.
 *
 * The terminal draws with WebGL only. xterm's DOM renderer injects <style>
 * elements, which the interface's Content-Security-Policy (style-src
 * 'self') blocks, and the CSP is not loosened for it. So the DOM renderer
 * is never used: without WebGL, or when the WebGL context is lost, the
 * console says it is unavailable instead.
 */
export function SerialConsole({ vmId }: { vmId: string }) {
  const host = useRef<HTMLDivElement>(null);
  const [status, setStatus] = useState<Status>("connecting");
  const [reason, setReason] = useState("");
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    const el = host.current;
    if (!el) return;
    const unavailable = (why: string) => {
      setStatus("unavailable");
      setReason(why);
    };
    // Checked before anything is created: xterm swallows a failure of the
    // WebGL renderer and falls back to the DOM renderer by itself.
    if (!webgl2Available()) {
      unavailable("This browser cannot give the console a WebGL 2 context (it may be turned off, or blocked for this graphics card).");
      return;
    }

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
    let ws: WebSocket | undefined;
    let resize: ResizeObserver | undefined;
    let disposed = false;
    const teardown = () => {
      if (disposed) return;
      disposed = true;
      resize?.disconnect();
      if (ws) {
        ws.onclose = null;
        ws.close();
      }
      term.dispose();
    };
    // Loaded before open(): the WebGL renderer is then in place when the
    // terminal opens, and xterm does not create its DOM renderer.
    const webgl = new WebglAddon();
    webgl.onContextLoss(() => {
      // The GPU took the context back. The DOM renderer is not an option,
      // so the console ends; Reconnect builds a new one.
      teardown();
      unavailable("The console lost its graphics context. Reconnect to open it again.");
    });
    try {
      term.loadAddon(webgl);
    } catch {
      term.dispose();
      unavailable("The console's WebGL renderer could not be loaded.");
      return;
    }
    term.open(el);
    // The WebGL renderer draws into its own canvas; without one, xterm
    // fell back to the DOM renderer, which must not be used.
    if (!el.querySelector(".xterm-screen canvas")) {
      teardown();
      unavailable("The console's WebGL renderer did not start.");
      return;
    }
    fit.fit();
    term.focus();

    ws = new WebSocket(consoleURL(vmId));
    ws.binaryType = "arraybuffer";
    const socket = ws;
    const encoder = new TextEncoder();
    socket.onopen = () => setStatus("open");
    socket.onmessage = (e: MessageEvent) => {
      if (e.data instanceof ArrayBuffer) term.write(new Uint8Array(e.data));
      else if (typeof e.data === "string") term.write(e.data);
    };
    socket.onclose = (e: CloseEvent) => {
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
      if (socket.readyState !== WebSocket.OPEN) return;
      socket.send(bytes);
      reportActivity();
    };
    term.onData((d) => send(encoder.encode(d)));
    term.onBinary((d) => send(Uint8Array.from(d, (c) => c.charCodeAt(0) & 0xff)));
    resize = new ResizeObserver(() => {
      try {
        fit.fit();
      } catch {
        // The element is being removed.
      }
    });
    resize.observe(el);

    return teardown;
  }, [vmId, attempt]);

  const reconnect = () => {
    setStatus("connecting");
    setReason("");
    setAttempt((n) => n + 1);
  };

  return (
    <div className="space-y-2">
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-2 text-xs text-muted-foreground">
          <Badge tone={status === "open" ? "ok" : status === "connecting" ? "info" : status === "unavailable" ? "warn" : "neutral"}>
            {status === "open" ? "connected" : status === "unavailable" ? "console unavailable" : status}
          </Badge>
          {status === "closed" ? <span role="status">{reason}</span> : null}
          {status === "open" || status === "connecting" ? <span>Serial console (COM1). Click it to type.</span> : null}
        </div>
        {status === "closed" || status === "unavailable" ? (
          <Button size="sm" variant="outline" onClick={reconnect}>
            Reconnect
          </Button>
        ) : null}
      </div>
      {status === "unavailable" ? (
        <Notice tone="warn" title="Console unavailable" role="status">
          {reason} The console needs WebGL 2. The VM itself is not affected.
        </Notice>
      ) : null}
      <div
        ref={host}
        data-testid="serial-console"
        className={status === "unavailable" ? "hidden" : "h-[28rem] overflow-hidden rounded-md border bg-[#0b0f14] p-2"}
        aria-label="Serial console"
      />
    </div>
  );
}
