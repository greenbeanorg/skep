#!/usr/bin/env python3
"""8c2-ptytype.py: run one command on a lab host over "ssh -tt" and answer
skep-api's password prompts ("New password for NAME:", "Repeat the
password:") with the password read from a 0600 file on merrick. The
password is written only to the pty; skep-api reads it without echo, so it
never appears in the output, which is printed as it comes.

Usage: 8c2-ptytype.py PWFILE HOST COMMAND...
Exit status: the remote command's.
"""
import os, pty, re, select, stat, sys

PROMPT = re.compile(rb"(New password for [^:\r\n]*:|Repeat the password:)\s*$")


def main():
    if len(sys.argv) < 4:
        sys.exit(__doc__)
    pwfile, host, cmd = os.path.expanduser(sys.argv[1]), sys.argv[2], sys.argv[3:]
    st = os.stat(pwfile)
    if stat.S_IMODE(st.st_mode) & 0o077:
        sys.exit(f"{pwfile} is readable by others")
    with open(pwfile, "rb") as f:
        pw = f.read().rstrip(b"\r\n")
    pid, fd = pty.fork()
    if pid == 0:
        os.execvp("ssh", ["ssh", "-tt", "-o", "BatchMode=yes", "aba@" + host, " ".join(cmd)])
    buf = b""
    answered = 0
    while True:
        try:
            r, _, _ = select.select([fd], [], [], 600)
        except InterruptedError:
            continue
        if not r:
            break
        try:
            data = os.read(fd, 4096)
        except OSError:
            break
        if not data:
            break
        sys.stdout.buffer.write(data)
        sys.stdout.flush()
        buf = (buf + data)[-512:]
        if PROMPT.search(buf):
            os.write(fd, pw + b"\n")
            answered += 1
            buf = b""
    pw = None
    _, status = os.waitpid(pid, 0)
    sys.stderr.write(f"[ptytype: {answered} prompt(s) answered]\n")
    sys.exit(os.waitstatus_to_exitcode(status))


if __name__ == "__main__":
    main()
