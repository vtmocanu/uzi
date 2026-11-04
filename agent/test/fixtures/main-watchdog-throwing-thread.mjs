// A watchdog thread that fails at load, to prove failure isolation.
throw new Error("boom SECRET-MARKER");
