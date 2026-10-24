# Patches to matter.js

Applied to `node_modules` by `patch-package` when `npm ci` runs (the `postinstall` script).

## @matter/node 0.17.9: subscribe again after a peer announces a shutdown

When a device sends the ShutDown event, matter.js's controller closes its sessions but keeps the subscription,
waiting for the device to resume it after the reboot. A device that doesn't persist subscriptions (this bridge)
never does, so the controller only re-subscribed at the subscription timeout, about 1m 44s. The patch gives the
device 30 seconds to resume, then closes the subscription so a new one is established.

Only a matter.js *controller* is affected: the bridge test (`test/e2e.mjs`), not the bridge itself or Google
Home, Apple Home and Alexa. Proposed upstream as "Subscribe again to a peer that announced a shutdown and does
not resume the subscription"; drop this patch once a matter.js release includes it.
