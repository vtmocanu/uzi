import { runPrivateCa } from "./private-ca.js";

runPrivateCa().catch((error: unknown) => {
  console.error(error);
  process.exitCode = 1;
});
