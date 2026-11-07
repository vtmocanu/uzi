import type { EffectiveSettings, StartPreview, StartPreviewRequest } from "../../lib/apiTypes";

// PRD #3254 M2: the recorded server behavior the demo serves, one entry per scenario. This is a
// verbatim copy of `scenarios` in fixtures/api-contract/effective_settings.behavior.json (written
// by the Go LiveDB test); mockApi.effectiveSettings.test.ts fails if the two drift, so regenerate
// by copying the recorded array rather than editing it by hand.
export interface RecordedStartPreview {
  name: string;
  request: StartPreviewRequest;
  response: StartPreview;
}
export interface RecordedEffectiveScenario {
  name: string;
  get: EffectiveSettings;
  start_previews: RecordedStartPreview[];
}

export const RECORDED_EFFECTIVE_SCENARIOS: RecordedEffectiveScenario[] = [
  {
    "name": "default",
    "get": {
      "version": 1,
      "harnesses": [
        {
          "harness": "claude",
          "usable": true,
          "refusal": null,
          "refusal_detail": null,
          "credential": {
            "kind": "anthropic",
            "source": "claim_selection",
            "fixed_at": "claim",
            "auth_mode": null
          }
        },
        {
          "harness": "codex",
          "usable": true,
          "refusal": null,
          "refusal_detail": null,
          "credential": {
            "kind": "codex",
            "source": "codex_default_credential",
            "fixed_at": "create",
            "auth_mode": "subscription"
          }
        }
      ],
      "start": [
        {
          "choice": "default",
          "harness": {
            "value": null,
            "source": "chosen_at_create",
            "fixed_at": "create",
            "notes": [
              "automatic_weekly_reset"
            ]
          },
          "refusal": null,
          "refusal_detail": null,
          "anthropic_override_allowed": true
        },
        {
          "choice": "claude",
          "harness": {
            "value": "claude",
            "source": "run_override",
            "fixed_at": "create",
            "notes": []
          },
          "refusal": null,
          "refusal_detail": null,
          "anthropic_override_allowed": true
        },
        {
          "choice": "codex",
          "harness": {
            "value": "codex",
            "source": "run_override",
            "fixed_at": "create",
            "notes": []
          },
          "refusal": null,
          "refusal_detail": null,
          "anthropic_override_allowed": false
        }
      ],
      "models": [
        {
          "harness": "claude",
          "model": null,
          "default_model": "opus",
          "default_source": "template",
          "resolved_model": "opus",
          "source": "template",
          "setting": "worker_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": []
        },
        {
          "harness": "codex",
          "model": null,
          "default_model": "gpt-6-sol",
          "default_source": "template",
          "resolved_model": "gpt-6-sol",
          "source": "template",
          "setting": "worker_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": []
        },
        {
          "harness": "claude",
          "model": null,
          "default_model": "opus",
          "default_source": "builtin",
          "resolved_model": "opus",
          "source": "builtin",
          "setting": "judge_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": []
        },
        {
          "harness": "codex",
          "model": null,
          "default_model": "gpt-6.1-sol",
          "default_source": "builtin",
          "resolved_model": "gpt-6.1-sol",
          "source": "builtin",
          "setting": "judge_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": []
        },
        {
          "harness": "claude",
          "model": null,
          "default_model": "haiku",
          "default_source": "builtin",
          "resolved_model": "haiku",
          "source": "builtin",
          "setting": "summary_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": []
        },
        {
          "harness": "codex",
          "model": null,
          "default_model": "gpt-6-luna",
          "default_source": "builtin",
          "resolved_model": "gpt-6-luna",
          "source": "builtin",
          "setting": "summary_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": []
        },
        {
          "harness": "claude",
          "model": null,
          "default_model": "",
          "default_source": "builtin",
          "resolved_model": "",
          "source": "builtin",
          "setting": "task_review_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": [
            "sdk_default"
          ]
        },
        {
          "harness": "codex",
          "model": null,
          "default_model": "gpt-6-sol",
          "default_source": "builtin",
          "resolved_model": "gpt-6-sol",
          "source": "builtin",
          "setting": "task_review_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": [
            "not_configurable"
          ]
        }
      ],
      "efforts": [
        {
          "harness": "claude",
          "effort": null,
          "default_effort": "medium",
          "default_source": "builtin",
          "resolved_effort": "medium",
          "source": "builtin",
          "mode": "explicit",
          "setting": "worker_effort",
          "fixed_at": "claim",
          "notes": []
        },
        {
          "harness": "codex",
          "effort": null,
          "default_effort": "medium",
          "default_source": "builtin",
          "resolved_effort": "medium",
          "source": "builtin",
          "mode": "explicit",
          "catalog_default": {
            "state": "known",
            "effort": "low",
            "model": "gpt-6-sol",
            "runtime_version": "0.164.0",
            "observed_at": "2026-01-02T03:04:05Z"
          },
          "setting": "worker_effort",
          "fixed_at": "claim",
          "notes": []
        },
        {
          "harness": "claude",
          "effort": null,
          "default_effort": "",
          "default_source": "builtin",
          "resolved_effort": null,
          "source": "builtin",
          "mode": "runtime_default",
          "setting": "task_review_effort",
          "fixed_at": "claim",
          "notes": [
            "not_sent"
          ]
        },
        {
          "harness": "codex",
          "effort": null,
          "default_effort": "",
          "default_source": "builtin",
          "resolved_effort": null,
          "source": "builtin",
          "mode": "runtime_default",
          "catalog_default": {
            "state": "known",
            "effort": "low",
            "model": "gpt-6-sol",
            "runtime_version": "0.164.0",
            "observed_at": "2026-01-02T03:04:05Z"
          },
          "setting": "task_review_effort",
          "fixed_at": "claim",
          "notes": [
            "not_sent"
          ]
        }
      ],
      "cross_check": {
        "models": [
          {
            "harness": "claude",
            "model": null,
            "default_model": "opus",
            "default_source": "cross_check_worker_default",
            "resolved_model": "opus",
            "source": "cross_check_worker_default",
            "setting": "cross_check_plan",
            "fixed_at": "claim",
            "ignored": null,
            "notes": []
          },
          {
            "harness": "codex",
            "model": null,
            "default_model": "gpt-6-sol",
            "default_source": "cross_check_worker_default",
            "resolved_model": "gpt-6-sol",
            "source": "cross_check_worker_default",
            "setting": "cross_check_plan",
            "fixed_at": "claim",
            "ignored": null,
            "notes": []
          },
          {
            "harness": "claude",
            "model": null,
            "default_model": "opus",
            "default_source": "cross_check_worker_default",
            "resolved_model": "opus",
            "source": "cross_check_worker_default",
            "setting": "cross_check_code",
            "fixed_at": "claim",
            "ignored": null,
            "notes": []
          },
          {
            "harness": "codex",
            "model": null,
            "default_model": "gpt-6-sol",
            "default_source": "cross_check_worker_default",
            "resolved_model": "gpt-6-sol",
            "source": "cross_check_worker_default",
            "setting": "cross_check_code",
            "fixed_at": "claim",
            "ignored": null,
            "notes": []
          }
        ],
        "efforts": [
          {
            "harness": "claude",
            "effort": null,
            "default_effort": "high",
            "default_source": "cross_check_default",
            "resolved_effort": "high",
            "source": "cross_check_default",
            "mode": "explicit",
            "setting": "cross_check_plan",
            "fixed_at": "claim",
            "notes": []
          },
          {
            "harness": "codex",
            "effort": null,
            "default_effort": "high",
            "default_source": "cross_check_default",
            "resolved_effort": "high",
            "source": "cross_check_default",
            "mode": "explicit",
            "setting": "cross_check_plan",
            "fixed_at": "claim",
            "notes": []
          },
          {
            "harness": "claude",
            "effort": null,
            "default_effort": "high",
            "default_source": "cross_check_default",
            "resolved_effort": "high",
            "source": "cross_check_default",
            "mode": "explicit",
            "setting": "cross_check_code",
            "fixed_at": "claim",
            "notes": []
          },
          {
            "harness": "codex",
            "effort": null,
            "default_effort": "high",
            "default_source": "cross_check_default",
            "resolved_effort": "high",
            "source": "cross_check_default",
            "mode": "explicit",
            "setting": "cross_check_code",
            "fixed_at": "claim",
            "notes": []
          }
        ]
      }
    },
    "start_previews": [
      {
        "name": "default start",
        "request": {
          "harness": null,
          "credential_override": null
        },
        "response": {
          "version": 1,
          "harness": {
            "value": null,
            "source": "chosen_at_create",
            "fixed_at": "create",
            "notes": [
              "automatic_weekly_reset"
            ]
          },
          "refusal": null,
          "refusal_detail": null,
          "credential": null,
          "alternatives": [
            {
              "harness": "claude",
              "credential": {
                "kind": "anthropic",
                "source": "claim_selection",
                "fixed_at": "claim",
                "auth_mode": null
              },
              "refusal": null,
              "refusal_detail": null
            },
            {
              "harness": "codex",
              "credential": {
                "kind": "codex",
                "source": "codex_default_credential",
                "fixed_at": "create",
                "auth_mode": "subscription"
              },
              "refusal": null,
              "refusal_detail": null
            }
          ]
        }
      },
      {
        "name": "claude",
        "request": {
          "harness": "claude",
          "credential_override": null
        },
        "response": {
          "version": 1,
          "harness": {
            "value": "claude",
            "source": "run_override",
            "fixed_at": "create",
            "notes": []
          },
          "refusal": null,
          "refusal_detail": null,
          "credential": {
            "kind": "anthropic",
            "source": "claim_selection",
            "fixed_at": "claim",
            "auth_mode": null
          }
        }
      },
      {
        "name": "codex",
        "request": {
          "harness": "codex",
          "credential_override": null
        },
        "response": {
          "version": 1,
          "harness": {
            "value": "codex",
            "source": "run_override",
            "fixed_at": "create",
            "notes": []
          },
          "refusal": null,
          "refusal_detail": null,
          "credential": {
            "kind": "codex",
            "source": "codex_default_credential",
            "fixed_at": "create",
            "auth_mode": "subscription"
          }
        }
      },
      {
        "name": "codex with a token override",
        "request": {
          "harness": "codex",
          "credential_override": {
            "mode": "auto"
          }
        },
        "response": {
          "version": 1,
          "harness": {
            "value": "codex",
            "source": "run_override",
            "fixed_at": "create",
            "notes": []
          },
          "refusal": "credential_override_harness_unsupported",
          "refusal_detail": null,
          "credential": null
        }
      },
      {
        "name": "inherit",
        "request": {
          "harness": null,
          "credential_override": {
            "mode": "inherit"
          }
        },
        "response": {
          "version": 1,
          "harness": {
            "value": null,
            "source": "chosen_at_create",
            "fixed_at": "create",
            "notes": [
              "automatic_weekly_reset"
            ]
          },
          "refusal": null,
          "refusal_detail": null,
          "credential": null,
          "alternatives": [
            {
              "harness": "claude",
              "credential": {
                "kind": "anthropic",
                "source": "claim_selection",
                "fixed_at": "claim",
                "auth_mode": null
              },
              "refusal": null,
              "refusal_detail": null
            },
            {
              "harness": "codex",
              "credential": {
                "kind": "codex",
                "source": "codex_default_credential",
                "fixed_at": "create",
                "auth_mode": "subscription"
              },
              "refusal": null,
              "refusal_detail": null
            }
          ]
        }
      },
      {
        "name": "auto token",
        "request": {
          "harness": null,
          "credential_override": {
            "mode": "auto"
          }
        },
        "response": {
          "version": 1,
          "harness": {
            "value": "claude",
            "source": "automatic",
            "fixed_at": "create",
            "notes": []
          },
          "refusal": null,
          "refusal_detail": null,
          "credential": {
            "kind": "anthropic",
            "source": "auto_selection",
            "fixed_at": "claim",
            "auth_mode": null
          }
        }
      },
      {
        "name": "enabled pinned token",
        "request": {
          "harness": null,
          "credential_override": {
            "mode": "pinned",
            "secret_id": "00000000-0000-4000-8000-0000000000a1"
          }
        },
        "response": {
          "version": 1,
          "harness": {
            "value": "claude",
            "source": "automatic",
            "fixed_at": "create",
            "notes": []
          },
          "refusal": null,
          "refusal_detail": null,
          "credential": {
            "kind": "anthropic",
            "source": "token_override",
            "fixed_at": "create",
            "auth_mode": null
          }
        }
      },
      {
        "name": "disabled pinned token",
        "request": {
          "harness": null,
          "credential_override": {
            "mode": "pinned",
            "secret_id": "00000000-0000-4000-8000-0000000000a2"
          }
        },
        "response": {
          "version": 1,
          "harness": {
            "value": "claude",
            "source": "automatic",
            "fixed_at": "create",
            "notes": []
          },
          "refusal": "credential_disabled",
          "refusal_detail": null,
          "credential": null
        }
      },
      {
        "name": "missing pinned token",
        "request": {
          "harness": null,
          "credential_override": {
            "mode": "pinned",
            "secret_id": "00000000-0000-4000-8000-0000000000a3"
          }
        },
        "response": {
          "version": 1,
          "harness": {
            "value": "claude",
            "source": "automatic",
            "fixed_at": "create",
            "notes": []
          },
          "refusal": "credential_not_found",
          "refusal_detail": null,
          "credential": null
        }
      },
      {
        "name": "pinned without a token",
        "request": {
          "harness": null,
          "credential_override": {
            "mode": "pinned"
          }
        },
        "response": {
          "version": 1,
          "harness": {
            "value": "claude",
            "source": "automatic",
            "fixed_at": "create",
            "notes": []
          },
          "refusal": "credential_override_invalid",
          "refusal_detail": null,
          "credential": null
        }
      }
    ]
  },
  {
    "name": "codexOnly",
    "get": {
      "version": 1,
      "harnesses": [
        {
          "harness": "claude",
          "usable": false,
          "refusal": "no_credential_for_harness",
          "refusal_detail": null,
          "credential": null
        },
        {
          "harness": "codex",
          "usable": true,
          "refusal": null,
          "refusal_detail": null,
          "credential": {
            "kind": "codex",
            "source": "codex_default_credential",
            "fixed_at": "create",
            "auth_mode": "subscription"
          }
        }
      ],
      "start": [
        {
          "choice": "default",
          "harness": {
            "value": "codex",
            "source": "automatic",
            "fixed_at": "create",
            "notes": []
          },
          "refusal": null,
          "refusal_detail": null,
          "anthropic_override_allowed": false
        },
        {
          "choice": "claude",
          "harness": null,
          "refusal": "no_credential_for_harness",
          "refusal_detail": null,
          "anthropic_override_allowed": false
        },
        {
          "choice": "codex",
          "harness": {
            "value": "codex",
            "source": "run_override",
            "fixed_at": "create",
            "notes": []
          },
          "refusal": null,
          "refusal_detail": null,
          "anthropic_override_allowed": false
        }
      ],
      "models": [
        {
          "harness": "claude",
          "model": null,
          "default_model": "",
          "default_source": "builtin",
          "resolved_model": "",
          "source": "builtin",
          "setting": "worker_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": [
            "sdk_default"
          ]
        },
        {
          "harness": "codex",
          "model": null,
          "default_model": "gpt-6.1-sol",
          "default_source": "builtin",
          "resolved_model": "gpt-6.1-sol",
          "source": "builtin",
          "setting": "worker_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": []
        },
        {
          "harness": "claude",
          "model": null,
          "default_model": "opus",
          "default_source": "builtin",
          "resolved_model": "opus",
          "source": "builtin",
          "setting": "judge_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": []
        },
        {
          "harness": "codex",
          "model": null,
          "default_model": "gpt-6.1-sol",
          "default_source": "builtin",
          "resolved_model": "gpt-6.1-sol",
          "source": "builtin",
          "setting": "judge_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": []
        },
        {
          "harness": "claude",
          "model": null,
          "default_model": "haiku",
          "default_source": "builtin",
          "resolved_model": "haiku",
          "source": "builtin",
          "setting": "summary_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": []
        },
        {
          "harness": "codex",
          "model": null,
          "default_model": "gpt-6-luna",
          "default_source": "builtin",
          "resolved_model": "gpt-6-luna",
          "source": "builtin",
          "setting": "summary_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": []
        },
        {
          "harness": "claude",
          "model": null,
          "default_model": "",
          "default_source": "builtin",
          "resolved_model": "",
          "source": "builtin",
          "setting": "task_review_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": [
            "sdk_default"
          ]
        },
        {
          "harness": "codex",
          "model": null,
          "default_model": "gpt-6-sol",
          "default_source": "builtin",
          "resolved_model": "gpt-6-sol",
          "source": "builtin",
          "setting": "task_review_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": [
            "not_configurable"
          ]
        }
      ],
      "efforts": [
        {
          "harness": "claude",
          "effort": null,
          "default_effort": "medium",
          "default_source": "builtin",
          "resolved_effort": "medium",
          "source": "builtin",
          "mode": "explicit",
          "setting": "worker_effort",
          "fixed_at": "claim",
          "notes": []
        },
        {
          "harness": "codex",
          "effort": null,
          "default_effort": "medium",
          "default_source": "builtin",
          "resolved_effort": "medium",
          "source": "builtin",
          "mode": "explicit",
          "catalog_default": {
            "state": "unknown",
            "effort": null,
            "model": null,
            "runtime_version": null,
            "observed_at": null
          },
          "setting": "worker_effort",
          "fixed_at": "claim",
          "notes": []
        },
        {
          "harness": "claude",
          "effort": null,
          "default_effort": "",
          "default_source": "builtin",
          "resolved_effort": null,
          "source": "builtin",
          "mode": "runtime_default",
          "setting": "task_review_effort",
          "fixed_at": "claim",
          "notes": [
            "not_sent"
          ]
        },
        {
          "harness": "codex",
          "effort": null,
          "default_effort": "",
          "default_source": "builtin",
          "resolved_effort": null,
          "source": "builtin",
          "mode": "runtime_default",
          "catalog_default": {
            "state": "unknown",
            "effort": null,
            "model": null,
            "runtime_version": null,
            "observed_at": null
          },
          "setting": "task_review_effort",
          "fixed_at": "claim",
          "notes": [
            "not_sent"
          ]
        }
      ],
      "cross_check": {
        "models": [
          {
            "harness": "claude",
            "model": null,
            "default_model": "",
            "default_source": "cross_check_worker_default",
            "resolved_model": "",
            "source": "cross_check_worker_default",
            "setting": "cross_check_plan",
            "fixed_at": "claim",
            "ignored": null,
            "notes": [
              "sdk_default"
            ]
          },
          {
            "harness": "codex",
            "model": null,
            "default_model": "gpt-6.1-sol",
            "default_source": "cross_check_worker_default",
            "resolved_model": "gpt-6.1-sol",
            "source": "cross_check_worker_default",
            "setting": "cross_check_plan",
            "fixed_at": "claim",
            "ignored": null,
            "notes": []
          },
          {
            "harness": "claude",
            "model": null,
            "default_model": "",
            "default_source": "cross_check_worker_default",
            "resolved_model": "",
            "source": "cross_check_worker_default",
            "setting": "cross_check_code",
            "fixed_at": "claim",
            "ignored": null,
            "notes": [
              "sdk_default"
            ]
          },
          {
            "harness": "codex",
            "model": null,
            "default_model": "gpt-6.1-sol",
            "default_source": "cross_check_worker_default",
            "resolved_model": "gpt-6.1-sol",
            "source": "cross_check_worker_default",
            "setting": "cross_check_code",
            "fixed_at": "claim",
            "ignored": null,
            "notes": []
          }
        ],
        "efforts": [
          {
            "harness": "claude",
            "effort": null,
            "default_effort": "high",
            "default_source": "cross_check_default",
            "resolved_effort": "high",
            "source": "cross_check_default",
            "mode": "explicit",
            "setting": "cross_check_plan",
            "fixed_at": "claim",
            "notes": []
          },
          {
            "harness": "codex",
            "effort": null,
            "default_effort": "high",
            "default_source": "cross_check_default",
            "resolved_effort": "high",
            "source": "cross_check_default",
            "mode": "explicit",
            "setting": "cross_check_plan",
            "fixed_at": "claim",
            "notes": []
          },
          {
            "harness": "claude",
            "effort": null,
            "default_effort": "high",
            "default_source": "cross_check_default",
            "resolved_effort": "high",
            "source": "cross_check_default",
            "mode": "explicit",
            "setting": "cross_check_code",
            "fixed_at": "claim",
            "notes": []
          },
          {
            "harness": "codex",
            "effort": null,
            "default_effort": "high",
            "default_source": "cross_check_default",
            "resolved_effort": "high",
            "source": "cross_check_default",
            "mode": "explicit",
            "setting": "cross_check_code",
            "fixed_at": "claim",
            "notes": []
          }
        ]
      }
    },
    "start_previews": [
      {
        "name": "default start",
        "request": {
          "harness": null,
          "credential_override": null
        },
        "response": {
          "version": 1,
          "harness": {
            "value": "codex",
            "source": "automatic",
            "fixed_at": "create",
            "notes": []
          },
          "refusal": null,
          "refusal_detail": null,
          "credential": {
            "kind": "codex",
            "source": "codex_default_credential",
            "fixed_at": "create",
            "auth_mode": "subscription"
          }
        }
      },
      {
        "name": "claude",
        "request": {
          "harness": "claude",
          "credential_override": null
        },
        "response": {
          "version": 1,
          "harness": null,
          "refusal": "no_credential_for_harness",
          "refusal_detail": null,
          "credential": null
        }
      },
      {
        "name": "codex",
        "request": {
          "harness": "codex",
          "credential_override": null
        },
        "response": {
          "version": 1,
          "harness": {
            "value": "codex",
            "source": "run_override",
            "fixed_at": "create",
            "notes": []
          },
          "refusal": null,
          "refusal_detail": null,
          "credential": {
            "kind": "codex",
            "source": "codex_default_credential",
            "fixed_at": "create",
            "auth_mode": "subscription"
          }
        }
      }
    ]
  },
  {
    "name": "overflowCodexDefault",
    "get": {
      "version": 1,
      "harnesses": [
        {
          "harness": "claude",
          "usable": true,
          "refusal": null,
          "refusal_detail": null,
          "credential": {
            "kind": "anthropic",
            "source": "claim_selection",
            "fixed_at": "claim",
            "auth_mode": null
          }
        },
        {
          "harness": "codex",
          "usable": true,
          "refusal": null,
          "refusal_detail": null,
          "credential": {
            "kind": "codex",
            "source": "codex_default_credential",
            "fixed_at": "create",
            "auth_mode": "subscription"
          }
        }
      ],
      "start": [
        {
          "choice": "default",
          "harness": {
            "value": null,
            "source": "chosen_at_create",
            "fixed_at": "create",
            "notes": [
              "overflow"
            ]
          },
          "refusal": null,
          "refusal_detail": null,
          "anthropic_override_allowed": true
        },
        {
          "choice": "claude",
          "harness": {
            "value": "claude",
            "source": "run_override",
            "fixed_at": "create",
            "notes": []
          },
          "refusal": null,
          "refusal_detail": null,
          "anthropic_override_allowed": true
        },
        {
          "choice": "codex",
          "harness": {
            "value": "codex",
            "source": "run_override",
            "fixed_at": "create",
            "notes": []
          },
          "refusal": null,
          "refusal_detail": null,
          "anthropic_override_allowed": false
        }
      ],
      "models": [
        {
          "harness": "claude",
          "model": null,
          "default_model": "opus",
          "default_source": "template",
          "resolved_model": "opus",
          "source": "template",
          "setting": "worker_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": []
        },
        {
          "harness": "codex",
          "model": null,
          "default_model": "gpt-6-sol",
          "default_source": "template",
          "resolved_model": "gpt-6-sol",
          "source": "template",
          "setting": "worker_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": []
        },
        {
          "harness": "claude",
          "model": null,
          "default_model": "opus",
          "default_source": "builtin",
          "resolved_model": "opus",
          "source": "builtin",
          "setting": "judge_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": []
        },
        {
          "harness": "codex",
          "model": null,
          "default_model": "gpt-6.1-sol",
          "default_source": "builtin",
          "resolved_model": "gpt-6.1-sol",
          "source": "builtin",
          "setting": "judge_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": []
        },
        {
          "harness": "claude",
          "model": null,
          "default_model": "haiku",
          "default_source": "builtin",
          "resolved_model": "haiku",
          "source": "builtin",
          "setting": "summary_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": []
        },
        {
          "harness": "codex",
          "model": null,
          "default_model": "gpt-6-luna",
          "default_source": "builtin",
          "resolved_model": "gpt-6-luna",
          "source": "builtin",
          "setting": "summary_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": []
        },
        {
          "harness": "claude",
          "model": null,
          "default_model": "",
          "default_source": "builtin",
          "resolved_model": "",
          "source": "builtin",
          "setting": "task_review_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": [
            "sdk_default"
          ]
        },
        {
          "harness": "codex",
          "model": null,
          "default_model": "gpt-6-sol",
          "default_source": "builtin",
          "resolved_model": "gpt-6-sol",
          "source": "builtin",
          "setting": "task_review_model",
          "fixed_at": "claim",
          "ignored": null,
          "notes": [
            "not_configurable"
          ]
        }
      ],
      "efforts": [
        {
          "harness": "claude",
          "effort": null,
          "default_effort": "medium",
          "default_source": "builtin",
          "resolved_effort": "medium",
          "source": "builtin",
          "mode": "explicit",
          "setting": "worker_effort",
          "fixed_at": "claim",
          "notes": []
        },
        {
          "harness": "codex",
          "effort": null,
          "default_effort": "medium",
          "default_source": "builtin",
          "resolved_effort": "medium",
          "source": "builtin",
          "mode": "explicit",
          "catalog_default": {
            "state": "known",
            "effort": "low",
            "model": "gpt-6-sol",
            "runtime_version": "0.164.0",
            "observed_at": "2026-01-02T03:04:05Z"
          },
          "setting": "worker_effort",
          "fixed_at": "claim",
          "notes": []
        },
        {
          "harness": "claude",
          "effort": null,
          "default_effort": "",
          "default_source": "builtin",
          "resolved_effort": null,
          "source": "builtin",
          "mode": "runtime_default",
          "setting": "task_review_effort",
          "fixed_at": "claim",
          "notes": [
            "not_sent"
          ]
        },
        {
          "harness": "codex",
          "effort": null,
          "default_effort": "",
          "default_source": "builtin",
          "resolved_effort": null,
          "source": "builtin",
          "mode": "runtime_default",
          "catalog_default": {
            "state": "known",
            "effort": "low",
            "model": "gpt-6-sol",
            "runtime_version": "0.164.0",
            "observed_at": "2026-01-02T03:04:05Z"
          },
          "setting": "task_review_effort",
          "fixed_at": "claim",
          "notes": [
            "not_sent"
          ]
        }
      ],
      "cross_check": {
        "models": [
          {
            "harness": "claude",
            "model": null,
            "default_model": "opus",
            "default_source": "cross_check_worker_default",
            "resolved_model": "opus",
            "source": "cross_check_worker_default",
            "setting": "cross_check_plan",
            "fixed_at": "claim",
            "ignored": null,
            "notes": []
          },
          {
            "harness": "codex",
            "model": null,
            "default_model": "gpt-6-sol",
            "default_source": "cross_check_worker_default",
            "resolved_model": "gpt-6-sol",
            "source": "cross_check_worker_default",
            "setting": "cross_check_plan",
            "fixed_at": "claim",
            "ignored": null,
            "notes": []
          },
          {
            "harness": "claude",
            "model": null,
            "default_model": "opus",
            "default_source": "cross_check_worker_default",
            "resolved_model": "opus",
            "source": "cross_check_worker_default",
            "setting": "cross_check_code",
            "fixed_at": "claim",
            "ignored": null,
            "notes": []
          },
          {
            "harness": "codex",
            "model": null,
            "default_model": "gpt-6-sol",
            "default_source": "cross_check_worker_default",
            "resolved_model": "gpt-6-sol",
            "source": "cross_check_worker_default",
            "setting": "cross_check_code",
            "fixed_at": "claim",
            "ignored": null,
            "notes": []
          }
        ],
        "efforts": [
          {
            "harness": "claude",
            "effort": null,
            "default_effort": "high",
            "default_source": "cross_check_default",
            "resolved_effort": "high",
            "source": "cross_check_default",
            "mode": "explicit",
            "setting": "cross_check_plan",
            "fixed_at": "claim",
            "notes": []
          },
          {
            "harness": "codex",
            "effort": null,
            "default_effort": "high",
            "default_source": "cross_check_default",
            "resolved_effort": "high",
            "source": "cross_check_default",
            "mode": "explicit",
            "setting": "cross_check_plan",
            "fixed_at": "claim",
            "notes": []
          },
          {
            "harness": "claude",
            "effort": null,
            "default_effort": "high",
            "default_source": "cross_check_default",
            "resolved_effort": "high",
            "source": "cross_check_default",
            "mode": "explicit",
            "setting": "cross_check_code",
            "fixed_at": "claim",
            "notes": []
          },
          {
            "harness": "codex",
            "effort": null,
            "default_effort": "high",
            "default_source": "cross_check_default",
            "resolved_effort": "high",
            "source": "cross_check_default",
            "mode": "explicit",
            "setting": "cross_check_code",
            "fixed_at": "claim",
            "notes": []
          }
        ]
      }
    },
    "start_previews": [
      {
        "name": "default start",
        "request": {
          "harness": null,
          "credential_override": null
        },
        "response": {
          "version": 1,
          "harness": {
            "value": null,
            "source": "chosen_at_create",
            "fixed_at": "create",
            "notes": [
              "overflow"
            ]
          },
          "refusal": null,
          "refusal_detail": null,
          "credential": null,
          "alternatives": [
            {
              "harness": "claude",
              "credential": {
                "kind": "anthropic",
                "source": "claim_selection",
                "fixed_at": "claim",
                "auth_mode": null
              },
              "refusal": null,
              "refusal_detail": null
            },
            {
              "harness": "codex",
              "credential": {
                "kind": "codex",
                "source": "codex_default_credential",
                "fixed_at": "create",
                "auth_mode": "subscription"
              },
              "refusal": null,
              "refusal_detail": null
            }
          ]
        }
      },
      {
        "name": "inherit",
        "request": {
          "harness": null,
          "credential_override": {
            "mode": "inherit"
          }
        },
        "response": {
          "version": 1,
          "harness": {
            "value": null,
            "source": "chosen_at_create",
            "fixed_at": "create",
            "notes": [
              "overflow"
            ]
          },
          "refusal": null,
          "refusal_detail": null,
          "credential": null,
          "alternatives": [
            {
              "harness": "claude",
              "credential": {
                "kind": "anthropic",
                "source": "claim_selection",
                "fixed_at": "claim",
                "auth_mode": null
              },
              "refusal": null,
              "refusal_detail": null
            },
            {
              "harness": "codex",
              "credential": null,
              "refusal": "credential_override_harness_unsupported",
              "refusal_detail": null
            }
          ]
        }
      }
    ]
  }
];
