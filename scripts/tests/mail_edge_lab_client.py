#!/usr/bin/env python3
# =============================================================================
# scripts/tests/mail_edge_lab_client.py — the mail client of test-mail-edge-lab.sh
# =============================================================================
"""The lab's mail client, run inside a container on one of its two networks.
Certificates are not verified: the lab's is self-signed."""
import imaplib, smtplib, socket, ssl, sys, time
ctx = ssl.create_default_context(); ctx.check_hostname = False; ctx.verify_mode = ssl.CERT_NONE

def smtp(host, port, sender, rcpt, user=None, pw=None, starttls=True):
    s = smtplib.SMTP(host, int(port), timeout=20); s.ehlo("client.example")
    if starttls:
        s.starttls(context=ctx); s.ehlo("client.example")
        print("tls:", s.sock.version())
    if user:
        s.login(user, pw); print("auth: ok as", user)
    try:
        s.sendmail(sender, [rcpt], f"From: {sender}\r\nTo: {rcpt}\r\nSubject: lab {time.time()}\r\n\r\nhello\r\n")
        print("accepted:", sender, "->", rcpt)
    except smtplib.SMTPRecipientsRefused as e:
        print("refused:", {k: (v[0], v[1].decode()) for k, v in e.recipients.items()})
    s.quit()

def imaps(host, port, user, pw):
    m = imaplib.IMAP4_SSL(host, int(port), ssl_context=ctx, timeout=20)
    print("login:", m.login(user, pw)[0]); typ, data = m.select("INBOX"); print("inbox messages:", data[0].decode()); m.logout()

def raw(host, port, header=None, wait=8):
    s = socket.create_connection((host, int(port)), timeout=wait)
    if header: s.sendall(header.encode().decode("unicode_escape").encode())
    try:
        data = s.recv(200); print("got:", repr(data[:80]) if data else "closed without a byte")
    except socket.timeout: print("got: nothing in", wait, "s")
    except ConnectionError as e: print("got:", type(e).__name__)
    s.close()

def rawtls(host, port):
    s = socket.create_connection((host, int(port)), timeout=8)
    try:
        t = ctx.wrap_socket(s); print("tls up:", t.recv(100))
    except Exception as e: print("got:", type(e).__name__, str(e)[:60])

def _open(host, port):
    """One connection: 'ok' (a 220 banner), 'server' (the server answered and refused), 'proxy' (closed without a byte)."""
    try:
        s = socket.create_connection((host, int(port)), timeout=5); s.settimeout(6)
        data = s.recv(100)
    except Exception:
        return "proxy", None
    if data.startswith(b"220"): return "ok", s
    s.close(); return ("server" if data else "proxy"), None

def _report(what, n, counts):
    print(f"{what} {n}: {counts['ok']} got a 220 banner, {counts['server']} were refused by the server with an SMTP answer, "
          f"{counts['proxy']} were closed by the proxy without a byte")

def flood(host, port, n):
    """n connections held open at once."""
    held, counts = [], {"ok": 0, "server": 0, "proxy": 0}
    for i in range(int(n)):
        kind, s = _open(host, port); counts[kind] += 1
        if s: held.append(s)
    _report("held open at once,", n, counts)
    for s in held: s.close()

def burst(host, port, n):
    """n connections one after the other, each closed before the next."""
    counts = {"ok": 0, "server": 0, "proxy": 0}
    for i in range(int(n)):
        kind, s = _open(host, port); counts[kind] += 1
        if s: s.close()
    _report("one after the other,", n, counts)

globals()[sys.argv[1]](*sys.argv[2:])
