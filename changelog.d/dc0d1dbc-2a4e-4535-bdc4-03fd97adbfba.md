### Fixed

- **A hosted worker's mid-run disk park now reaches recovery_wait instead of stalling (#3916).**
  The park capture no longer waits on the run's own disk-stop pause, and every mid-run data_volume_full park takes the three-capture bound, ending in a degraded park or worker_residue_blocked with custody kept.
