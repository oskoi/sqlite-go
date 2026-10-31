package main

import "C"

// The native allocator is paused only in its isolated backup-init probe. This
// observes the reservation while sqlite3_backup_init is actually inside C,
// without exposing library test controls or inspecting private handle fields.
var allocationEntered, allocationContinue chan struct{}

//export probe_go_allocation
func probe_go_allocation() {
	close(allocationEntered)
	<-allocationContinue
}
