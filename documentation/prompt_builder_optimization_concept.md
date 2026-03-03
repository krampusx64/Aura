# Prompt Builder Optimization Concept

## Status Quo — Analyse

### Aktueller Token-Verbrauch (System Prompt)

| Bereich | Chars | ~Tokens | Wann geladen |
|---|---|---|---|
| 01_identity.md | 877 | 219 | **Immer** |
| 02_personality.md (Extensions) | 1,418 | 355 | **Immer** |
| 03_tools_registry.md | 2,806 | 702 | **Immer** |
| Core Personality Profile | 355–1,087 | 89–272 | **Immer** (1 Profil) |
| core_memory.md | ~124 | 31 | **Immer** |
| PersonalityLine `[Self: mood=...]` | ~80 | 20 | **Immer** (wenn aktiv) |
| Language + DateTime Block | ~120 | 30 | **Immer** |
| **Minimum (immer-on)** | **~5,780** | **~1,445** | |
| | | | |
| RAG: Retrieved Memories | 0–2,000 | 0–500 | Wenn Treffer |
| Predicted Memories | 0–1,000 | 0–250 | Wenn temporal match |
| Tool Guides (max 3) | 0–3,500 | 0–875 | Wenn predicted |
| 04_lifeboat.md | 2,702 | 676 | Nur Maintenance |
| ctx_coding_guidelines.md | 1,293 | 323 | Nur bei Code |
| ctx_error_recovery.md | 549 | 137 | Nur bei Error |
| maintenance.md | 798 | 200 | Nur Maintenance |
| **Worst Case** | **~15,700** | **~3,925** | |

### Problem

Das Zielmodell `arcee-ai/trinity-large-preview:free` hat ein begrenztes Context Window (~8K tokens). Bei **~1,445 Tokens** Minimum-System-Prompt + Chat-History (typisch 6–12 Messages à ~200 Tokens = 1.200–2.400 Tokens) + RAG-Injektionen bleibt nur **~4K–5K** Tokens für die LLM-Antwort. Wenn Tool-Calls dazukommen (Tool-Result → +200–1.000 Tokens pro Call), wird der Kontext schnell voll und das Modell gibt leere Responses.

---

## Optimierungsplan — 7 Strategien

### Strategie 1: Token Budget System (HOCH)

**Konzept:** Statt alles blind reinzuwerfen, definieren wir ein festes Token-Budget für den System-Prompt und lassen den Builder intelligent kürzen.

```
config.yaml:
  agent:
    system_prompt_token_budget: 1200  # Max Tokens für den System-Prompt
```

**Implementierung in `builder.go`:**
1. Nach dem Assemblieren aller Module → `CountTokens(rawPrompt)` aufrufen
2. Wenn Budget überschritten → **Priority-basiertes Shedding**: Niedrigste Priority-Module werden rausgeworfen bis Budget eingehalten
3. RAG-Inhalte werden proportional getrimmt (z.B. nur Top-1 statt Top-3)
4. Tool Guides werden komplett geshedded wenn Platz fehlt

**Budget-Kaskade (Shedding-Reihenfolge):**
```
1. Tool Guides entfernen           (spart ~875 Tokens)
2. Predicted Memories entfernen    (spart ~250 Tokens)
3. Retrieved Memories trimmen      (spart ~250 Tokens)
4. 02_personality.md → Kurzversion (spart ~200 Tokens)
5. Personality Profile kürzen      (spart ~100 Tokens)
6. NIEMALS: 01_identity, 03_tools_registry  (Kernfunktion)
```

**Aufwand:** Mittel — neuer `budgetShed(modules, budget)` in builder.go  
**Impact:** HOCH — verhindert Context-Overflow proaktiv statt reaktiv

---

### Strategie 2: Prompt Compression / Deduplizierung (HOCH)

**Analyse der aktuellen Redundanzen:**

| Redundanz | Wo | Einsparung |
|---|---|---|
| Security-Regeln | 01_identity + 02_personality Punkt 4 | ~150 chars |
| Autonomy-Instruktionen | 02_personality Punkt 1 + 03_tools_registry AUTONOMY | ~200 chars |
| "Update before action" | 02_personality Punkt 5 + 03_tools_registry Regel 2 | ~150 chars |
| follow_up Limit "max 10" | 03_tools_registry Regel 3 + follow_up.md Manual | ~80 chars |

**Maßnahmen:**
1. **Merge** 01_identity + 02_personality → ein **01_core.md** (~1.600 statt 2.295 chars, **spart ~700 chars / ~175 Tokens**)
2. **03_tools_registry.md trimmen:** Core Tools Table enthält Mini-Schemas die redundant zu den Manuals sind → nur Name + 1-Wort-Beschreibung, Schema in Manuals belassen → **spart ~400 chars / ~100 Tokens**
3. **Natürliche Sprache → Stichpunkte:** Fließtext wie "You are a balanced, objective assistant..." → `Balanced. Factual. No emotions. Concise.` → **spart ~30% pro Profil**

**Aufwand:** Gering — reine Prompt-Textarbeit + 1 Datei-Zusammenlegung  
**Impact:** HOCH — ~275 Tokens Ersparnis bei jedem Request

---

### Strategie 3: Adaptive Prompt Tiers (MITTEL)

**Konzept:** Statt ein statisches Prompt zu bauen, 3 Tier-Stufen basierend auf verbleibendem Kontext:

| Tier | Trigger | System-Prompt |
|---|---|---|
| **Full** | messages ≤ 6 | Alles laden (Identity, Personality, Tools, RAG, Guides) |
| **Compact** | messages 7–12 | Kein RAG, keine Guides, gekürzte Personality |
| **Minimal** | messages > 12 | Nur Identity + Tools-Registry (Kernschema), keine Personality |

**Implementierung:**
```go
func determineTier(messageCount int) string {
    switch {
    case messageCount <= 6:
        return "full"
    case messageCount <= 12:
        return "compact"
    default:
        return "minimal"
    }
}
```

Der Builder bekommt `Tier string` als neues ContextFlags-Feld. Je nach Tier werden Module selektiv geladen.

**Aufwand:** Gering — ~30 Zeilen in builder.go + Tier-Berechnung in agent.go  
**Impact:** MITTEL — spart 400–800 Tokens im späteren Gesprächsverlauf

---

### Strategie 4: Lazy Tool Schema Injection (MITTEL)

**Problem:** `03_tools_registry.md` listet **alle** Tools mit Mini-Schemas (702 Tokens). Die meisten Tools werden in einer Konversation nie benutzt.

**Konzept:** 
1. `03_tools_registry.md` enthält nur noch die **Tool-Namen** als kompakte Liste (~150 Tokens statt 702)
2. **Erst wenn ein Tool benutzt wird**, wird dessen volles Schema in den nächsten System-Prompt injiziert
3. Builder trackt `RecentlyUsedTools []string` und injiziert nur deren Schemas

**Implementierung:**
```go
type ContextFlags struct {
    // ... existing ...
    RecentlyUsedTools []string  // Last 3 tools the agent used
}
```

Der Agent trackt die letzten 3 genutzten Tools. Der Builder injiziert nur deren Detail-Schemas + die kompakte Gesamtliste.

**Aufwand:** Mittel — Tools-Registry splitten + Tracking in agent.go  
**Impact:** MITTEL — spart ~550 Tokens wenn wenige Tools aktiv

---

### Strategie 5: Prompt Diff / Delta-Updates (NIEDRIG, KOMPLEX)

**Konzept:** Statt den System-Prompt bei jedem Turn komplett neu zu bauen, nur die Teile senden die sich geändert haben.

**Problem:** OpenAI API unterstützt kein Prompt-Delta. Der System-Prompt muss als Ganzes im Messages-Array stehen.

**Alternative (machbar):** 
- System-Prompt nur **alle 3–4 Turns** komplett neu bauen
- Zwischen den Turns: Nur `req.Messages[0].Content` beibehalten ohne Rebuild
- RAG-Ergebnisse nur beim ersten Turn + nach Tool-Calls injizieren, nicht bei jedem Turn

**Aufwand:** Gering — 1 Counter-Variable + Conditional in agent.go  
**Impact:** NIEDRIG — spart CPU, nicht Tokens (Prompt wird trotzdem gesendet)

---

### Strategie 6: Echtes Token-Counting statt char/4 (MITTEL)

**Problem:** `CountTokens()` ist eine Heuristik (`len(text) / 4`). Das ist ungenau — manche Tokens sind 1 Byte, manche 8+. Besonders bei Markdown-Syntax, Code-Blöcken und Nicht-ASCII-Zeichen weicht das stark ab.

**Lösung:** Integration von `tiktoken-go` oder einem leichtgewichtigen BPE-Tokenizer.

```go
import "github.com/pkoukk/tiktoken-go"

func CountTokens(text string) int {
    enc, err := tiktoken.GetEncoding("cl100k_base")
    if err != nil {
        return len(text) / 4 // Fallback
    }
    return len(enc.Encode(text, nil, nil))
}
```

**Vorteil:** Exaktes Token-Budget möglich → Strategie 1 wird damit erst richtig präzise.

**Aufwand:** Gering — 1 Dependency + ~10 Zeilen  
**Impact:** MITTEL — enabler für alle Token-Budget-Features

---

### Strategie 7: Context Window Auto-Detection (NIEDRIG)

**Konzept:** Beim Start das Context Window des Modells abfragen (via OpenRouter API) und alle Budgets automatisch berechnen.

```
GET https://openrouter.ai/api/v1/models
→ context_length: 8192
→ system_prompt_budget = context_length * 0.20  = 1638 tokens
→ history_budget       = context_length * 0.50  = 4096 tokens
→ response_budget      = context_length * 0.30  = 2458 tokens
```

**Aufwand:** Gering — 1 API-Call beim Start, Ergebnisse in Config cachen  
**Impact:** NIEDRIG — Nice-to-have, macht Strategie 1 selbst-konfigurierend

---

## Priorisierte Umsetzungsreihenfolge

| Phase | Strategie | Aufwand | Token-Einsparung | Beschreibung |
|---|---|---|---|---|
| **Phase 1** | S2: Deduplizierung | 1–2h | ~275 Tokens | Prompts zusammenlegen + kürzen |
| **Phase 2** | S6: Echtes Token-Counting | 30min | enabler | tiktoken-go integrieren |
| **Phase 3** | S1: Token Budget System | 2–3h | dynamisch | Budget-Kaskade mit Priority-Shedding |
| **Phase 4** | S3: Adaptive Tiers | 1h | 400–800 Tokens | 3-Tier System basierend auf Message-Count |
| **Phase 5** | S4: Lazy Tool Schemas | 2h | ~550 Tokens | Tool-Registry aufsplitten |
| **Phase 6** | S7: Auto-Detection | 1h | Automatisierung | Context Window beim Start abfragen |

**Gesamtpotential:** Von ~1.445 Tokens Minimum auf ~800–900 Tokens Minimum (ca. **40% Reduktion**), plus dynamisches Shedding bei langen Konversationen.

---

## Erwartetes Ergebnis

```
VORHER (Worst Case):
System Prompt: ~3,900 Tokens (27% von 8K budget)
History:       ~3,000 Tokens 
Response:      ~1,100 Tokens ← ZU WENIG → Empty Response

NACHHER (mit Phase 1–4):
System Prompt: ~1,000 Tokens (7% von 8K budget)  
History:       ~3,000 Tokens
Response:      ~4,000 Tokens ← KOMFORTABEL
```

---

## Architektur-Diagramm

```
┌─────────────────────────────────────────────────────┐
│                    agent.go                          │
│  messageCount → determineTier() → tier              │
│  lastTools    → recentlyUsedTools                   │
│                     │                               │
│                     ▼                               │
│  ┌─────────────────────────────────────────────┐    │
│  │           BuildSystemPrompt()                │   │
│  │                                              │   │
│  │  1. Load Modules (filtered by tier)          │   │
│  │  2. Inject Dynamic Content (RAG, Memory)     │   │
│  │  3. Count Tokens (tiktoken)                  │   │
│  │  4. Budget Check                             │   │
│  │     ├─ Under Budget → return                 │   │
│  │     └─ Over Budget → budgetShed()            │   │
│  │         ├─ Drop Tool Guides                  │   │
│  │         ├─ Drop Predicted Memories           │   │
│  │         ├─ Trim Retrieved Memories           │   │
│  │         ├─ Compact Personality               │   │
│  │         └─ Emergency: Minimal Mode           │   │
│  │  5. OptimizePrompt() (whitespace, comments)  │   │
│  │  6. Return final prompt                      │   │
│  └─────────────────────────────────────────────┘    │
│                     │                               │
│                     ▼                               │
│           Token Budget: 1200 (configurable)         │
│           Actual:        ~950                       │
│           History:      ~3000                       │
│           Free for LLM: ~4000+ ✓                    │
└─────────────────────────────────────────────────────┘
```

---

## Nicht-Empfohlen

| Idee | Warum nicht |
|---|---|
| Prompt in anderem Format (JSON statt Markdown) | Markdown ist für LLMs nativer und token-effizienter als JSON |
| System-Prompt komprimieren/zippen | API akzeptiert nur Plaintext |
| Prompt in User-Message statt System | Manche Modelle ignorieren dann die Instruktionen |
| Alle Prompts in eine Datei | Verliert Modularität, Wartbarkeit sinkt |
