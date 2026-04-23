import SwiftUI

/// App palette. SwiftUI's default `.primary` in dark mode is pure
/// white on pure black (21:1 contrast) — technically WCAG-compliant
/// but triggers halation / shimmer for long reads and for readers with
/// astigmatism (≈30% of the population). We target Material Design's
/// ~87% primary / ~60% secondary / ~38% tertiary pattern on a soft
/// `#121212` background — ratio settles around 12:1, still well above
/// WCAG AAA (7:1), but visually calm.
enum Palette {
    /// Primary body text — ≈ #E5E5E5.
    static let primaryText = Color(white: 0.90)
    /// Secondary / meta text — ≈ #A6A6A6.
    static let secondaryText = Color(white: 0.65)
    /// Tertiary / timestamps — ≈ #737373.
    static let tertiaryText = Color(white: 0.45)
    /// Base background — ≈ #121212 (Material's dark-surface zero).
    static let background = Color(white: 0.07)
    /// Elevated surface for cards / composer — ≈ #1C1C1C.
    static let surface = Color(white: 0.11)
    /// Accent blue — matches the existing SwiftUI tint hue but slightly muted.
    static let accent = Color(red: 0.33, green: 0.52, blue: 0.98)
    /// Error / destructive text — soft red, not pure.
    static let error = Color(red: 0.94, green: 0.50, blue: 0.50)
}
