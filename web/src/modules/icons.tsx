// The fixed icon vocabulary a module's `icon` field may name (PRD #4686). A module cannot
// ship an icon: it picks one of the host's own glyphs by name, so the nav stays visually
// consistent and nothing from a module's list reaches the DOM as markup. The Helm chart and
// the compose generator keep their own copy of this list; a parity test pins them equal.

import type { ReactNode } from "react";
import {
  ActivityIcon,
  BoardIcon,
  BookIcon,
  BotIcon,
  BugIcon,
  ChatIcon,
  ClockIcon,
  FactoryIcon,
  FileTextIcon,
  GearIcon,
  GitIcon,
  HomeIcon,
  KeyIcon,
  PackageIcon,
  ScaleIcon,
  SearchIcon,
  ServerIcon,
  ShieldIcon,
  SkillIcon,
  TerminalIcon,
} from "../components/icons";

const MODULE_ICONS = {
  activity: ActivityIcon,
  board: BoardIcon,
  book: BookIcon,
  bot: BotIcon,
  bug: BugIcon,
  chat: ChatIcon,
  clock: ClockIcon,
  factory: FactoryIcon,
  "file-text": FileTextIcon,
  gear: GearIcon,
  git: GitIcon,
  home: HomeIcon,
  key: KeyIcon,
  package: PackageIcon,
  scale: ScaleIcon,
  search: SearchIcon,
  server: ServerIcon,
  shield: ShieldIcon,
  skill: SkillIcon,
  terminal: TerminalIcon,
} as const;

export type ModuleIconName = keyof typeof MODULE_ICONS;

export const MODULE_ICON_NAMES = Object.keys(MODULE_ICONS) as ModuleIconName[];

export function isModuleIconName(v: unknown): v is ModuleIconName {
  return typeof v === "string" && Object.prototype.hasOwnProperty.call(MODULE_ICONS, v);
}

export function moduleIcon(name: ModuleIconName): ReactNode {
  const Glyph = MODULE_ICONS[name];
  return <Glyph />;
}
