# Security

## Reporting

Report a vulnerability privately through
[GitHub Security Advisories](https://github.com/charbelkassab/pyrite/security/advisories/new),
not as a public issue.

## What the threat model actually is

pyrite runs on your machine, binds to `127.0.0.1` by default, has no accounts,
no users and no database, and sends nothing anywhere except to the data and
model providers you configure. That removes most of what a web application
usually has to worry about, and leaves two things that matter.

**Strategy code is executed.** Generated JavaScript runs in
[goja](https://github.com/dop251/goja), a pure-Go interpreter with no
filesystem, network, process or timer access. The only capabilities a strategy
has are the ones explicitly attached to the `ctx` object, and each call is
counted, capped and recorded. A sandbox escape — reaching the filesystem,
opening a socket, or running for longer than the per-session watchdog allows —
is a vulnerability. Please report it.

**Serving beyond localhost.** `--addr 0.0.0.0:8080` exposes an interface with
no authentication that will compile and execute code on request. That is
intended for a container you control on a network you control. Do not put it
on the public internet.

Your API keys are read from the environment, never written to disk by pyrite,
and never serialised into a saved run or the JSON API.

## Supported versions

The latest release. This is a research tool, not infrastructure — fixes go
into the next release rather than into patches of older ones.
