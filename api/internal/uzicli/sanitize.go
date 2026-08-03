package uzicli

import "gitlab.example.com/vtmocanu/uzi/api/internal/termsafe"

// This file used to HOLD the CLI's terminal-safety predicate (#180). It now delegates:
// the predicate, both renderers and their reasoning moved to api/internal/termsafe when
// #169's validator half landed, because api/internal/handler needs the same rule and a
// handler importing a CLI package is backwards layering. Read termsafe's package comment
// for the threat model and for the biconditional the two halves are pinned to.
//
// The wrappers stay rather than the call sites being rewritten, on purpose. Printer.Table,
// Printer.Printf, Printer.Println and every site in package main call these names; #180's
// render path is reviewed and its tests assert against this surface, so a mechanical
// rename across it would be diff nobody asked for over code that is already correct.

// SanitizeTTY strips the control characters that let untrusted text drive a terminal,
// while sparing \t and \n so multi-line free text still renders as the author wrote it.
// Use it for FLOWING text; use CellText for anything landing in a fixed-width column.
func SanitizeTTY(s string) string { return termsafe.SanitizeTTY(s) }

// CellText is SanitizeTTY plus the two folds a COLUMN needs (tab and newline to a space,
// then a trim), for text landing in a table cell. It deliberately does not truncate.
func CellText(s string) string { return termsafe.CellText(s) }
