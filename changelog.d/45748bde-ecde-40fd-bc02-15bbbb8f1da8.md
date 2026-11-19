### Fixed

- **Pod-epoch reclaim and the hibernation census prove clones on group-writable managed bares (#4486).**
  Admitted only under a startup runner-group trust probe and per-store ownership, ancestor, filesystem and ACL checks; worker images now ship acl (getfacl), which changes the runtime image key and rolls the fleet, and older images keep refusing as before.
