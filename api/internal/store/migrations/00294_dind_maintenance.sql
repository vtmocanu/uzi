-- +goose Up
ALTER TABLE workers
 ADD COLUMN dind_register_floor timestamptz NOT NULL DEFAULT now(),
 ADD COLUMN dind_meter_epoch bigint NOT NULL DEFAULT 0,
 ADD COLUMN dind_meter_at timestamptz,
 ADD COLUMN dind_pressure_streak integer NOT NULL DEFAULT 0,
 ADD COLUMN dind_meter_over boolean NOT NULL DEFAULT false,
 ADD COLUMN dind_below_threshold boolean NOT NULL DEFAULT false,
 ADD COLUMN maintenance_owns_drain boolean NOT NULL DEFAULT false,
 ADD COLUMN nix_pressure boolean NOT NULL DEFAULT false,
 ADD COLUMN data_pressure boolean NOT NULL DEFAULT false,
 ADD COLUMN maintenance_id uuid,
 ADD COLUMN maintenance_nonce text NOT NULL DEFAULT '',
 ADD COLUMN maintenance_phase text NOT NULL DEFAULT '' CHECK (maintenance_phase IN ('', 'requested', 'ready', 'stopping', 'recycling', 'complete', 'cancelled')),
 ADD COLUMN maintenance_deployment_uid text NOT NULL DEFAULT '',
 ADD COLUMN maintenance_pvc_uid text NOT NULL DEFAULT '',
 ADD COLUMN maintenance_register_nonce text NOT NULL DEFAULT '',
 ADD COLUMN maintenance_fenced boolean NOT NULL DEFAULT false,
 ADD COLUMN maintenance_ready_ack boolean NOT NULL DEFAULT false,
 ADD COLUMN maintenance_ack_at timestamptz,
 ADD COLUMN maintenance_activity_floor timestamptz NOT NULL DEFAULT now();

-- +goose Down
ALTER TABLE workers
 DROP COLUMN dind_register_floor, DROP COLUMN dind_meter_epoch, DROP COLUMN dind_meter_at,
 DROP COLUMN dind_pressure_streak, DROP COLUMN dind_meter_over, DROP COLUMN dind_below_threshold,
 DROP COLUMN maintenance_owns_drain, DROP COLUMN nix_pressure, DROP COLUMN data_pressure,
 DROP COLUMN maintenance_id, DROP COLUMN maintenance_nonce, DROP COLUMN maintenance_phase,
 DROP COLUMN maintenance_deployment_uid, DROP COLUMN maintenance_pvc_uid,
 DROP COLUMN maintenance_register_nonce, DROP COLUMN maintenance_fenced,
 DROP COLUMN maintenance_ready_ack, DROP COLUMN maintenance_ack_at, DROP COLUMN maintenance_activity_floor;
