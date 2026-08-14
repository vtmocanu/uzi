// Settings → Access: programmatic access to uzi (the CLI-token lifecycle today;
// the home for any future access credential). Inside SettingsShell so it sits
// beside Account & tokens / Run defaults / Forge / Memory.

import { SettingsShell } from "../components/SettingsShell";
import { CliTokens } from "../components/CliTokens";

export function AccessSettings() {
  return (
    <SettingsShell description="Tokens for driving uzi from the terminal or CI, without a browser session.">
      <CliTokens />
    </SettingsShell>
  );
}
