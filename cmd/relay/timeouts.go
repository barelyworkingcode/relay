package main

import "time"

const StatusPollInterval = 2 * time.Second

// RecoveryPollInterval paces listener re-convergence for a missed event.
const RecoveryPollInterval = 30 * time.Second
