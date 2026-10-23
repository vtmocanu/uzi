// Unbuffered diagnostic capture: retain short output even if the producer times out.
export const captureOutput = "/bin/dd bs=1 count=1048576 2>/dev/null";
