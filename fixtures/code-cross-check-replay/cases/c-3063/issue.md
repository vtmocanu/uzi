# Offer Send to uzi when filing a run finding through existing autopilot labels

When a maintainer files one run finding or a group of findings as a forge issue, there is no way to opt in to the existing uzi trigger labels, so a filed issue always needs a manual label step before an autopilot sweep can pick it up.

Add an explicit opt-in (a CLI flag and a web checkbox) that applies the already-configured trigger labels to the filed issue, with the consent recorded on the request. The default must stay off and an omitted field must keep today's behavior. Request types in the web client should mirror the Go request wire, including nullable fields.
