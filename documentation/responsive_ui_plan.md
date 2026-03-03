# AuraGo Web Interface – Responsive & Erweiterbar

## Status Quo Analyse

### Technologie-Stack aktuell
- **CSS**: Inline `<style>`-Blöcke mit CSS Custom Properties (Dark/Light Theme)
- **Tailwind CSS**: CDN geladen, aber kaum genutzt (nur `h-screen flex flex-col` auf `<body>`)
- **Kein Build-System**: Alles in einzelnen HTML-Dateien, embedded via Go `embed.FS`
- **JavaScript**: Vanilla JS inline, kein Framework

### Dateien
| Datei | Zeilen | Funktion |
|---|---|---|
| `ui/index.html` | 2066 | Chat-Interface (Header, Chat, Input, Modals, SSE) |
| `ui/config.html` | 972 | Konfigurations-Seite (Sidebar, Formular-Felder) |
| `ui/embed.go` | 7 | Go embed für statische Dateien |

### Bestehende Responsive-Ansätze
- `@media (max-width: 640px)` in index.html: Kleineres Bubble/Avatar, Header-Buttons mit `.desktop-only` ausgeblendet
- `@media (max-width: 768px)` in config.html: Sidebar auf 60px Icon-Only-Modus
- Mood-Widget wird auf Mobile komplett ausgeblendet

### Probleme
1. **Header-Overflow**: 9+ Elemente (Budget-Pill, Personality-Select, Mood-Widget, Token-Counter, Debug-Pill, Active-Pill, Theme-Toggle, Config-Link, Clear-Button) – überläuft auf Screens < 768px
2. **Keine Touch-Optimierung**: Tap-Targets teilweise < 44px
3. **Config-Sidebar**: Kein Hamburger-Menü, nur Icon-Collapse
4. **Kein Tablet-Breakpoint**: Sprung von Desktop direkt zu Mobile
5. **CSS-Duplikation**: Theme-Variablen und Header-Styles in beiden Dateien identisch kopiert
6. **Kein modulares CSS**: Alles inline, schwer wartbar bei wachsender UI

---

## Framework-Entscheidung

### Empfehlung: Tailwind CSS (bereits geladen) vollständig nutzen

**Begründung:**
- Tailwind CDN ist bereits in beiden Dateien eingebunden
- Kein Build-System nötig (CDN reicht für Go-embedded HTML)
- Responsive Utilities (`sm:`, `md:`, `lg:`) out-of-the-box
- Kompatibel mit bestehenden CSS Custom Properties
- Keine Migration nötig – schrittweise anwendbar

**NICHT empfohlen:**
- Bootstrap: Großes Bundle, eigenes Design-System kollidiert mit bestehendem Theme
- Material UI: Braucht React/Vue
- Vollständiger Tailwind-Build mit PostCSS: Unnötige Komplexität für Go-embedded Dateien

### Zusatz-Empfehlung: Tailwind-Config für Custom Theme

```html
<script>
tailwind.config = {
    theme: {
        extend: {
            colors: {
                'aura-accent': 'var(--accent)',
                'aura-bg': 'var(--bg-primary)',
                'aura-surface': 'var(--bg-secondary)',
                'aura-text': 'var(--text-primary)',
                'aura-muted': 'var(--text-secondary)',
            },
            fontFamily: {
                'sans': ['Inter', 'system-ui', '-apple-system', 'sans-serif'],
                'mono': ['SF Mono', 'Fira Code', 'Cascadia Code', 'monospace'],
            }
        }
    }
}
</script>
```

---

## Breakpoint-Strategie

| Breakpoint | Tailwind | Beschreibung |
|---|---|---|
| < 480px | Default (mobile-first) | Smartphone Portrait |
| ≥ 480px | `xs:` (custom) | Smartphone Landscape |
| ≥ 640px | `sm:` | Kleine Tablets |
| ≥ 768px | `md:` | Tablets |
| ≥ 1024px | `lg:` | Desktop |
| ≥ 1280px | `xl:` | Große Screens |

---

## Umsetzungsplan

### Phase 1: Shared Styles extrahieren (Grundlage)
**Ziel**: CSS-Duplikation beseitigen, gemeinsame Basis schaffen

**Aufgabe:**
1. Neue Datei `ui/shared.css` erstellen mit:
   - CSS Custom Properties (Theme-Variablen Dark + Light)
   - Reset/Base-Styles (box-sizing, scrollbar, body)
   - Shared Komponenten (Header, Logo, Buttons, Theme-Toggle)
2. `embed.go` um `shared.css` erweitern
3. Beide HTML-Dateien: Duplizierten CSS-Block durch `<link rel="stylesheet" href="/shared.css">` ersetzen
4. Tailwind-Config Block in beiden Dateien synchronisieren

**Erwarteter Effekt:**
- ~200 Zeilen CSS-Duplikation eliminiert
- Einheitliche Änderungen an einer Stelle möglich
- Grundlage für alle weiteren Phasen

---

### Phase 2: Chat-Interface (index.html) responsive machen

#### 2a. Header – Mobile-First Redesign

**Problem:** 9 Elemente passen nicht auf < 768px.

**Lösung: Overflow-Menü für Mobile**

```
Desktop (≥ 768px):
┌─────────────────────────────────────────────────────┐
│ 🤖 AURAGO  │ $0.03  🎭friendly  🔍curious  128tok  │
│             │ debug  Agent aktiv  ☀️  ⚙️  Neuer Chat │
└─────────────────────────────────────────────────────┘

Tablet (≥ 640px):
┌──────────────────────────────────────┐
│ 🤖 AURAGO  │  128tok  ☀️  ⚙️  ☰    │
└──────────────────────────────────────┘
  ☰ Drawer: Budget, Personality, Mood, Debug, Clear

Mobile (< 640px):
┌────────────────────────────┐
│ 🤖 AURAGO    │  ☀️  ☰     │
└────────────────────────────┘
  ☰ Drawer: Alle Header-Actions
```

**Umsetzung:**
1. Header-Actions in zwei Gruppen teilen:
   - **Immer sichtbar**: Logo, Theme-Toggle
   - **Overflow-Menü**: Alles andere (über `☰`-Button in einem Slide-Down-Panel)
2. CSS-Klassen:
   - `.header-always`: `display: flex` immer
   - `.header-overflow`: `hidden` auf Mobile, `flex` auf `md:`
3. Hamburger-Panel: Absolut positioniert unter Header, mit Animation
4. Touch-Target: Alle Buttons mindestens `44px × 44px`

#### 2b. Chat-Bereich

**Anpassungen:**
- Bubble `max-width`: `92%` auf Mobile, `80%` auf Desktop
- Avatar: `28px` auf Mobile (bereits vorhanden), `34px` ab `sm:`
- Padding `#chat-content`: `0.75rem` auf Mobile, `1.5rem 1rem` ab `sm:`
- Greeting-Icon: Kleiner auf Mobile (`48px` statt `64px`)
- Code-Blöcke in Bubbles: `font-size: 0.72rem` auf Mobile, horizontales Scrollen sicherstellen

#### 2c. Input-Bereich / Footer

**Anpassungen:**
- `gap: 0.35rem` auf Mobile statt `0.5rem`
- Upload-Button auf Mobile ausblenden (per Long-Press auf Send-Button oder separates Menü)
- Oder: Upload + Send als Icon-Buttons à `40px`, Stop-Button nur sichtbar wenn aktiv
- Textarea Mindesthöhe: `40px` statt `44px` auf Mobile
- Safe-Area-Inset beachten (iOS):
  ```css
  .app-footer {
      padding-bottom: max(0.75rem, env(safe-area-inset-bottom));
  }
  ```

#### 2d. Modal Dialog

- `width: 90%` → `width: calc(100% - 2rem)` auf Mobile
- `max-width: 380px` bleibt für Desktop
- Buttons vertikal statt horizontal auf sehr kleinen Screens

---

### Phase 3: Config-Seite (config.html) responsive machen

#### 3a. Sidebar → Mobile Sliding Drawer

**Problem:** 60px Icon-Only ist auf Mobile nicht optimal – kein visuelles Feedback welche Sektion aktiv ist.

**Lösung:**
```
Mobile (< 768px):
┌───────────────────────────┐
│ ⚡ AURAGO CONFIG  │  ☰   │  ← Hamburger statt Sidebar
└───────────────────────────┘
│        Content-Bereich     │
│                            │

Wenn ☰ geklickt:
┌───────────────────────────┐
│ ⚡ AURAGO CONFIG  │  ✕   │
├──────────────┬────────────┤
│  Sidebar     │            │
│  (overlay)   │  dimmed    │
│  280px       │  content   │
│              │            │
└──────────────┴────────────┘
```

**Umsetzung:**
1. Hamburger-Button nur auf `< md:` sichtbar
2. Sidebar: `position: fixed; left: -280px; transition: left 0.3s` auf Mobile
3. Overlay-Backdrop wenn Sidebar offen
4. Auto-Close bei Sektion-Klick auf Mobile
5. Swipe-Geste (optional): Von links nach rechts zum Öffnen

#### 3b. Formular-Felder

- Field-Groups: Volle Breite, `padding: 0.75rem` statt `1rem 1.2rem` auf Mobile
- Content-Padding: `1rem` statt `2rem 2.5rem` auf Mobile
- Toggle-Switches: Touch-freundlich, bereits `44px` hoch ✓
- Save-Bar: Sticky bottom, volle Breite auf Mobile

#### 3c. Restart-Dialog
- Ausreichend große Touch-Targets
- Vollbreite-Buttons auf Mobile

---

### Phase 4: Visual Upgrade (Bonus)

#### 4a. Glassmorphism verfeinern
```css
/* Stärkerer Glass-Effekt für Cards & Panels */
.glass-card {
    background: rgba(255, 255, 255, 0.03);
    backdrop-filter: blur(16px) saturate(1.2);
    border: 1px solid rgba(255, 255, 255, 0.06);
    box-shadow: 
        0 8px 32px rgba(0, 0, 0, 0.12),
        inset 0 1px 0 rgba(255, 255, 255, 0.05);
}
```

#### 4b. Animationen & Micro-Interactions
- **Page-Transitions**: Sanfte Fade-Animation beim Seitenwechsel (Chat ↔ Config)
- **Hover-States**: Subtiler Scale + Glow für interaktive Elemente
- **Skeleton-Loading**: Statt "Lade Konfiguration..." ein Shimmer-Effekt
- **Smooth Scroll**: Bereits vorhanden, aber Scroll-Indikator im Chat hinzufügen
- **Toast-Notifications**: Statt inline Status-Text für Save-Feedback

#### 4c. Typographie-Verbesserungen
```css
/* Fluid Typography */
:root {
    --text-base: clamp(0.875rem, 0.8rem + 0.25vw, 1rem);
    --text-sm: clamp(0.75rem, 0.7rem + 0.2vw, 0.85rem);
    --text-lg: clamp(1.1rem, 1rem + 0.3vw, 1.3rem);
}
```

#### 4d. Accent-Color Warmth
- Subtilen warmen Gradient in der Hintergrund-Textur
- Accent-Farbe leicht animiert (Hue-Shift im Idle-State der Logo-Icon)

#### 4e. Dark-Mode Polish
- Tiefere Schatten mit farbiger Kante (colored shadows)
- Subtile Gradient-Borders statt flacher Borders

---

### Phase 5: Erweiterbarkeit sichern

#### 5a. CSS Component Architecture

Dateistruktur (alle via `embed.go` eingebettet):
```
ui/
├── shared.css          # Variablen, Reset, Shared Components
├── index.html          # Chat – nur page-spezifisches CSS
├── config.html         # Config – nur page-spezifisches CSS
├── embed.go            # Go embed directive
└── *.png               # Assets
```

#### 5b. CSS Custom Property Convention
```css
/* Naming: --{component}-{property}-{variant} */
--header-bg: ...;
--header-height: ...;
--bubble-max-width: ...;
--bubble-radius: ...;
--input-height: ...;
--sidebar-width: ...;
--sidebar-width-collapsed: ...;
```

#### 5c. Tailwind Utility-First für neue Komponenten
- Neue UI-Elemente primär mit Tailwind-Klassen bauen
- Custom CSS nur für komplexe Animationen und Theme-Variablen
- Bestehende Komponenten schrittweise migrieren (nicht auf einmal)

#### 5d. JavaScript-Modularisierung (Zukunft)
- EventSource/SSE-Handler als separates Modul
- Chat-Rendering als separates Modul
- Aktuell nicht kritisch, aber bei nächster größerer Änderung lohnend

---

## Priorisierte Reihenfolge

| # | Phase | Impact | Aufwand | Priorität |
|---|---|---|---|---|
| 1 | Shared Styles extrahieren | Wartbarkeit ⬆ | ~1h | 🔴 Hoch |
| 2a | Header responsive | UX Mobile ⬆⬆ | ~2h | 🔴 Hoch |
| 2c | Footer/Input responsive | UX Mobile ⬆⬆ | ~1h | 🔴 Hoch |
| 2b | Chat-Bereich responsive | UX Mobile ⬆ | ~1h | 🟡 Mittel |
| 3a | Config Sidebar Drawer | UX Mobile ⬆⬆ | ~2h | 🟡 Mittel |
| 3b | Config Formular responsive | UX Mobile ⬆ | ~1h | 🟡 Mittel |
| 2d | Modal responsive | UX Mobile ⬆ | ~30min | 🟡 Mittel |
| 4a-e | Visual Upgrade | Look & Feel ⬆⬆ | ~3h | 🟢 Nice-to-have |
| 5 | Erweiterbarkeit | Maintainability ⬆⬆ | ~1h | 🟢 Nice-to-have |

**Gesamtaufwand: ~12-13 Stunden**

---

## Testplan

1. **Chrome DevTools Device Mode**: iPhone SE, iPhone 14, iPad, iPad Pro, Desktop
2. **Reale Geräte**: Mindestens 1 Android + 1 iOS testen
3. **Breakpoint-Checks**: Fenstergröße stufenweise von 320px bis 1440px durchfahren
4. **Touch-Targets**: Alle interaktiven Elemente ≥ 44×44px auf Mobile
5. **Landscape-Modus**: Chat + Config auf Smartphone-Landscape prüfen
6. **Theme-Wechsel**: Dark ↔ Light auf allen Breakpoints testen
7. **Performance**: Keine Layout-Shifts (CLS) beim Resize
