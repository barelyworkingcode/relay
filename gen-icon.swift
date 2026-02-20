#!/usr/bin/env swift
// Generates Relay.app icon: relay-node symbol (o--O--o) on a blue gradient background.
// Usage: swift gen-icon.swift <output-dir>
//   Creates <output-dir>/AppIcon.icns

import Cocoa

let outputDir = CommandLine.arguments.count > 1 ? CommandLine.arguments[1] : "."
let iconsetPath = "\(outputDir)/AppIcon.iconset"

try? FileManager.default.createDirectory(atPath: iconsetPath, withIntermediateDirectories: true)

// All required sizes for macOS .icns
let sizes: [(name: String, px: Int)] = [
    ("icon_16x16", 16),
    ("icon_16x16@2x", 32),
    ("icon_32x32", 32),
    ("icon_32x32@2x", 64),
    ("icon_128x128", 128),
    ("icon_128x128@2x", 256),
    ("icon_256x256", 256),
    ("icon_256x256@2x", 512),
    ("icon_512x512", 512),
    ("icon_512x512@2x", 1024),
]

func drawIcon(size: Int) -> NSImage {
    let s = CGFloat(size)
    let img = NSImage(size: NSSize(width: s, height: s))
    img.lockFocus()
    let ctx = NSGraphicsContext.current!.cgContext

    // --- Background: rounded super-ellipse with gradient ---
    let cornerRadius = s * 0.22
    let bgRect = CGRect(x: 0, y: 0, width: s, height: s)
    let bgPath = NSBezierPath(roundedRect: bgRect, xRadius: cornerRadius, yRadius: cornerRadius)

    // Blue gradient (top-left to bottom-right)
    ctx.saveGState()
    bgPath.addClip()
    let colors = [
        CGColor(red: 0.20, green: 0.56, blue: 0.98, alpha: 1.0),  // #3390FA
        CGColor(red: 0.30, green: 0.38, blue: 0.85, alpha: 1.0),  // #4D61D9
    ]
    let gradient = CGGradient(colorsSpace: CGColorSpaceCreateDeviceRGB(), colors: colors as CFArray, locations: [0.0, 1.0])!
    ctx.drawLinearGradient(gradient, start: CGPoint(x: 0, y: s), end: CGPoint(x: s, y: 0), options: [])
    ctx.restoreGState()

    // --- Draw relay symbol in white ---
    let cx = s / 2
    let cy = s / 2

    // Sizing relative to icon
    let hubRadius = s * 0.17
    let endpointRadius = s * 0.085
    let lineThickness = s * 0.08
    let endpointOffset = s * 0.32  // distance from center to endpoints

    ctx.setFillColor(CGColor(red: 1, green: 1, blue: 1, alpha: 0.95))

    // Central hub
    let hubRect = CGRect(x: cx - hubRadius, y: cy - hubRadius, width: hubRadius * 2, height: hubRadius * 2)
    ctx.fillEllipse(in: hubRect)

    // Left endpoint
    let lx = cx - endpointOffset
    let leftRect = CGRect(x: lx - endpointRadius, y: cy - endpointRadius, width: endpointRadius * 2, height: endpointRadius * 2)
    ctx.fillEllipse(in: leftRect)

    // Right endpoint
    let rx = cx + endpointOffset
    let rightRect = CGRect(x: rx - endpointRadius, y: cy - endpointRadius, width: endpointRadius * 2, height: endpointRadius * 2)
    ctx.fillEllipse(in: rightRect)

    // Left connecting line
    let leftLineRect = CGRect(x: lx + endpointRadius, y: cy - lineThickness / 2, width: (cx - hubRadius) - (lx + endpointRadius), height: lineThickness)
    ctx.fill(leftLineRect)

    // Right connecting line
    let rightLineRect = CGRect(x: cx + hubRadius, y: cy - lineThickness / 2, width: (rx - endpointRadius) - (cx + hubRadius), height: lineThickness)
    ctx.fill(rightLineRect)

    // Subtle inner detail on hub: small inner ring to add depth
    ctx.setFillColor(CGColor(red: 1, green: 1, blue: 1, alpha: 0.3))
    let innerRadius = hubRadius * 0.45
    let innerRect = CGRect(x: cx - innerRadius, y: cy - innerRadius, width: innerRadius * 2, height: innerRadius * 2)
    ctx.fillEllipse(in: innerRect)

    img.unlockFocus()
    return img
}

for (name, px) in sizes {
    let img = drawIcon(size: px)
    let tiff = img.tiffRepresentation!
    let rep = NSBitmapImageRep(data: tiff)!
    let png = rep.representation(using: .png, properties: [:])!
    let path = "\(iconsetPath)/\(name).png"
    try! png.write(to: URL(fileURLWithPath: path))
}

// Convert iconset to icns
let task = Process()
task.executableURL = URL(fileURLWithPath: "/usr/bin/iconutil")
task.arguments = ["-c", "icns", iconsetPath, "-o", "\(outputDir)/AppIcon.icns"]
try! task.run()
task.waitUntilExit()

// Clean up iconset
try? FileManager.default.removeItem(atPath: iconsetPath)

if task.terminationStatus == 0 {
    print("Generated \(outputDir)/AppIcon.icns")
} else {
    print("iconutil failed with status \(task.terminationStatus)")
    exit(1)
}
