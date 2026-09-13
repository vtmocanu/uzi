<!--
Liveness canary for scripts/check-spec-numbering.sh (issue #181, freeze issue #1317).
Two plants, ON PURPOSE: the duplicate `## 7.` heading proves the duplicate detector
fires, and `## 638.` proves the above-the-frozen-head detector fires. Do NOT "fix"
either: if this file stops carrying both, the gate exits 2 (instrument broken).
This file holds no secrets.
-->

## 3. First real section

## 7. First seven

## 7. Duplicated on purpose

## 12. Later section

## 638. Planted above the frozen head
