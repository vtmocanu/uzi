<!--
Liveness canary for scripts/check-spec-numbering.sh (issue #181).
The duplicate `## 7.` heading below is PLANTED ON PURPOSE: it is what proves the
duplicate detector actually fires. Do NOT "fix" it by renumbering -- if this file
stops containing a duplicate section number, the gate exits 2 (instrument broken).
This file holds no secrets.
-->

## 3. First real section

## 7. First seven

## 7. Duplicated on purpose

## 12. Later section
