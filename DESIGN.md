# Design System

This document specifies the visual theme, typography, color system, and UI components used in the Pixel Auth Admin dashboard.

## Color System
We use a modern, high-contrast Slate & Indigo palette representing premium SaaS platforms.

| Token | Hex Value | Role |
| --- | --- | --- |
| `--bg-main` | `#f8fafc` | Global body background (Slate 50) |
| `--bg-card` | `#ffffff` | Surface background for cards, tables, and modals |
| `--bg-highlight` | `#f1f5f9` | Background for hover states and subtle highlights (Slate 100) |
| `--primary` | `#4f46e5` | Core brand identity accent (Indigo 600) |
| `--primary-hover` | `#4338ca` | Darker indigo for button hovers |
| `--primary-light` | `#eeebff` | Indigo tint for active menu selections |
| `--text-primary` | `#0f172a` | Primary text color (Slate 900) |
| `--text-secondary` | `#475569` | Secondary text color (Slate 600) |
| `--border-color` | `#cbd5e1` | Thin border outlines (Slate 300) |
| `--success` | `#10b981` | Positive states and dot indicators |
| `--danger` | `#ef4444` | Errors and critical cancellation states |
| `--warning` | `#f59e0b` | Pending operations and queues |

## Typography
- **Body & Form Interface**: `-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif`
  - Body text uses clean sans-serif with normal spacing for general readability.
  - Form inputs and text fields use a clean `13px` sizing to ensure a high information density suitable for desktop workloads.
- **Monospace Elements (Keys, IDs, Raw Data)**: `"JetBrains Mono", Menlo, Monaco, monospace`
  - Displayed inside light-tint containers with high contrast for absolute legibility during manual audits.

## Layout & Components
- **Cards**: Flat borders (`1px solid var(--border-color)`) with a `12px` border-radius and extremely soft shadows (`var(--shadow-sm)`).
- **Buttons**:
  - **Primary**: Solid indigo gradients with fine white top borders for a modern tactile feel.
  - **Secondary / Cancel**: Light slate gradients with thin borders.
- **Status Badges**: Semi-transparent rounded pills containing a colored dot indicator. The "Running" state includes a pulsing loop animation for state feedback.
- **Background Details**: A subtle, slate-colored dot grid pattern on the body background adding visual depth to the platform.
