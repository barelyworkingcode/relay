// Package coverage checks that the feature map, the door catalogue, the
// reference documents and the end-to-end tests describe the same product. It
// reads documents and Go syntax only and imports nothing of relay's.
//
// Check applies the feature-map rules R1 to R9, the reference-heading rule
// (Q6) and the harness rules H1 to H3. Every miss is one Finding; a miss never
// stops the scan.
package coverage
