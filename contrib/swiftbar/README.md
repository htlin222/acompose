# acompose in your menu bar (SwiftBar / xbar)

`acompose menubar` prints a [SwiftBar](https://github.com/swiftbar/SwiftBar)/xbar
menu: a `⛴ running/total` title, one line per service with its status dot and
IP, per-service start/stop actions, published-port links, and whole-stack
up/down/refresh actions.

<!-- screenshot placeholder: assets/swiftbar.png -->

Install:

1. `brew install swiftbar` (or xbar) and pick a plugin folder.
2. Copy `acompose.5s.sh` into that folder (the `5s` is the refresh interval).
3. Edit `PROJECT_DIR` in the script to point at your compose project.
