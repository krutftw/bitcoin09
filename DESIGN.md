---
name: BTC09 Wallet
description: A self-custody payment instrument built like inspectable hardware.
colors:
  carbon-ink: "#111713"
  muted-readout: "#58625d"
  service-white: "#eef2ee"
  panel-gray: "#f8faf7"
  panel-white: "#ffffff"
  trace-gray: "#c2cbc4"
  trace-strong: "#8b9a91"
  solder-mask: "#123c32"
  solder-mask-deep: "#0d2f28"
  board-ink: "#f3f6f1"
  board-muted: "#b8cbc3"
  copper-mark: "#b35632"
  copper-deep: "#833b24"
  connected-signal: "#1c6b50"
  fault-signal: "#8f342e"
  caution-signal: "#8b5b20"
typography:
  display:
    fontFamily: "Segoe UI Variable, Segoe UI, system-ui, sans-serif"
    fontSize: "clamp(48px, 6vw, 68px)"
    fontWeight: 800
    lineHeight: 0.95
    letterSpacing: "-0.03em"
  headline:
    fontFamily: "Segoe UI Variable, Segoe UI, system-ui, sans-serif"
    fontSize: "clamp(26px, 8vw, 32px)"
    fontWeight: 750
    lineHeight: 1.05
    letterSpacing: "-0.02em"
  title:
    fontFamily: "Segoe UI Variable, Segoe UI, system-ui, sans-serif"
    fontSize: "18px"
    fontWeight: 750
    lineHeight: 1.2
  body:
    fontFamily: "Segoe UI Variable, Segoe UI, system-ui, sans-serif"
    fontSize: "16px"
    fontWeight: 400
    lineHeight: 1.5
  label:
    fontFamily: "Cascadia Mono, SFMono-Regular, Consolas, monospace"
    fontSize: "11px"
    fontWeight: 700
    lineHeight: 1.4
    letterSpacing: "0.12em"
rounded:
  square: "0"
  control: "2px"
  surface: "3px"
  status: "999px"
spacing:
  trace: "4px"
  compact: "8px"
  control: "12px"
  standard: "16px"
  section: "24px"
  panel: "32px"
  desktop: "48px"
components:
  button-primary:
    backgroundColor: "{colors.carbon-ink}"
    textColor: "{colors.panel-white}"
    typography: "{typography.title}"
    rounded: "{rounded.control}"
    padding: "14px 18px"
    height: "52px"
  button-secondary:
    backgroundColor: "{colors.panel-gray}"
    textColor: "{colors.carbon-ink}"
    typography: "{typography.title}"
    rounded: "{rounded.control}"
    padding: "13px 18px"
    height: "48px"
  input:
    backgroundColor: "{colors.panel-white}"
    textColor: "{colors.carbon-ink}"
    typography: "{typography.body}"
    rounded: "{rounded.control}"
    padding: "12px 14px"
    height: "50px"
  status-pill:
    backgroundColor: "{colors.panel-gray}"
    textColor: "{colors.connected-signal}"
    typography: "{typography.label}"
    rounded: "{rounded.status}"
    padding: "8px 12px"
  instrument-panel:
    backgroundColor: "{colors.solder-mask}"
    textColor: "{colors.board-ink}"
    rounded: "{rounded.control}"
    padding: "32px"
---

# Design System: BTC09 Wallet

## Overview

**Creative North Star: "The Inspectable Instrument"**

BTC09 Wallet should feel like a purpose-built payment instrument rather than a token portfolio or a miniature marketing site. Its visual world comes from circuit-board silkscreen, bench instruments, and service documentation: every line indicates structure, every status has a label, and important actions visibly lead to their consequence.

The experience stays composed and calm under normal use. Expression comes from disciplined geometry, the square `09` mark, trace-like dividers, instrument labels, and purposeful state changes—not gradients, glossy cards, decorative coin imagery, or generic fintech softness.

**Key Characteristics:**

- Cool mineral surfaces with one established copper accent.
- Flat, inspectable panels separated by trace lines and tonal fields.
- Square or lightly relieved controls with unmistakable states.
- Dense enough for desktop, direct enough for a phone.
- Tabular amounts and readable technical identifiers.

## Colors

The palette is restrained: cool workspace neutrals, carbon ink, deep solder-mask green, the existing copper `09` accent, and explicit status colors.

### Primary

- **Solder Mask:** deep mineral green for the navigation rail, major balance field, and selected structural regions.
- **Copper Mark:** the existing BTC09 copper for the `09` mark, primary action edge, focus treatment, and rare points requiring immediate attention.

### Secondary

- **Connected Signal:** a clean medium green used with a text label for healthy network and confirmed incoming states.
- **Fault Signal:** a dark oxide red used with a text label for errors and destructive warnings.

### Neutral

- **Service White:** cool near-white workspace ground.
- **Panel Gray:** slightly deeper surface for grouped controls and secondary regions.
- **Carbon Ink:** near-black text and strong controls.
- **Trace Gray:** cool gray-green dividers and quiet boundaries.

**The Copper Limit Rule.** Copper identifies the product and decisive action; it never becomes a decorative wash.

## Typography

**Display Font:** the platform UI stack, with tabular-number features for balances.
**Body Font:** the platform UI stack.
**Label/Mono Font:** the platform monospace stack for addresses, transaction IDs, check codes, heights, and instrument labels.

**Character:** Plain-language text reads like operating instructions. Amounts and chain evidence align like instrument readouts. The `09` mark may retain its established serif numerals, but serif display typography does not spread into the interface.

### Hierarchy

- **Display:** heavy, compact, tabular balance readout; large only where the amount is the task.
- **Headline:** restrained screen titles with no promotional flourish.
- **Title:** firm row and panel labels.
- **Body:** normal platform text with comfortable line height and short measures.
- **Label:** compact uppercase or monospace annotation used sparingly for status and structure.

**The Readout Rule.** Numeric values align; technical strings wrap or truncate deliberately; neither is shrunk into illegibility.

## Layout

Desktop uses a fixed instrument rail and a broad working area. Home places balance and primary actions in the dominant field, with activity treated as a ledger rather than a stack of cards. Send, receive, activity, and settings use the same rail and a centered working column with room for evidence beside the primary task.

Phone layouts return the rail to a bottom navigation bar, keep one primary column, respect safe areas, and preserve 48 px minimum targets. The wide layout must use the available window rather than centering a phone-sized strip.

Spacing follows a compact 4 px base rhythm with larger 16, 24, and 32 px steps defining panels and screen sections.

## Elevation & Depth

The system is flat by default. Depth comes from adjacent tonal fields, border weight, inset rules, and the occasional offset edge on the square product mark. Shadows are not a general card treatment.

**The Bench Rule.** If a surface appears raised, it must be because the user can act on it or because it is temporarily above the working plane.

## Shapes

Controls and panels use square geometry with very small corner relief. Major regions may carry one clipped or notched corner, echoing a board outline without compromising hit areas. Pills are reserved for compact status indicators where their capsule form improves recognition.

Circular shapes belong only to LEDs, radio controls, and other genuinely circular indicators. The square `09` mark must not be turned into a generic coin.

## Components

### Buttons

- **Primary:** carbon control with board-white text, a 2 px corner relief, and a minimum 48 px target. Hover lifts the surface tone; focus uses a visible copper outline.
- **Secondary:** panel-white control with a strong trace border. It carries supporting actions without looking disabled.
- **Action:** large receive and send controls live inside the solder-mask balance field and pair a copper-outlined icon with a plain text verb.

### Status Indicators

- **Network pill:** a compact capsule is the only recurring pill. It combines an LED dot with a written state so color never carries meaning alone.
- **Transaction state:** direction icon, written kind, confirmation state, shortened identifier, and signed amount read as one ledger row.

### Cards / Containers

- **Instrument panel:** deep solder-mask field for the available balance and immediate actions.
- **Content panel:** service-white or panel-white surface with a trace border. Containers remain flat and use 2–3 px corner relief.
- **Warning panel:** a restrained signal tint, border, icon, and direct consequence. It is not used for routine help.

### Inputs / Fields

- **Default:** panel-white field, strong trace border, 2 px corner relief, and full-size readable text.
- **Focus:** a copper outline outside the field preserves its measured geometry.
- **Technical values:** addresses, transaction IDs, and check codes use the label/mono face and wrap rather than shrinking.

### Navigation

Desktop keeps a solder-mask instrument rail visible through wallet, activity, settings, send, receive, and review. Phone layouts collapse the same three top-level destinations into a bottom navigation bar while detail screens use an explicit Back or Edit action.

## Do's and Don'ts

### Do:

- **Do** show the current network and payment state in words as well as color.
- **Do** use trace lines to clarify hierarchy and relationships.
- **Do** let the desktop app feel like desktop software.
- **Do** reserve the largest type for balances and irreversible review moments.
- **Do** retain familiar send, receive, activity, backup, and settings affordances.

### Don't:

- **Don't** build a dark neon crypto dashboard.
- **Don't** use warm cream cards, glass panels, floating gradients, or ornamental glow.
- **Don't** decorate with generated coin renders or blockchain stock imagery.
- **Don't** hide transaction consequences behind animation or technical jargon.
- **Don't** introduce remote fonts, analytics, or assets that weaken the wallet's local security boundary.
