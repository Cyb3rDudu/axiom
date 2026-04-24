/**
 * BibliographyWidget (#51/#56)
 *
 * Compact list of structured references for a draft. Scoped to what the
 * backend exposes: list, delete, migrate-from-markdown (preview + commit).
 * A full add/edit form comes in a follow-up — the writer produces new
 * entries automatically via content-block:references, so the panel's
 * primary job right now is visibility + pruning.
 */

import { useEffect, useState } from 'react';
import {
  Trash2, RefreshCw, BookOpen, AlertCircle, FileCheck2, RotateCcw,
} from 'lucide-react';
import { Button } from '../../../components/ui/button';
import { Card, CardContent, CardHeader, CardTitle } from '../../../components/ui/card';
import { MathMarkdown } from '../../../components/markdown/MathMarkdown';
import {
  clearWritingPortfolio,
  commitBibliographyMigration,
  deleteStructuredReference,
  generateWritingPortfolio,
  getSessionDraft,
  getStructuredReferences,
  previewBibliographyMigration,
  type MigrationPreview,
  type PortfolioOutput,
  type StructuredReference,
} from '../api';

interface BibliographyWidgetProps {
  draftId: string;
  /** When provided, the widget reads portfolio_output from the live
   *  session draft via getSessionDraft; otherwise the Portfolio section
   *  stays collapsed. Passed from DraftPanel which already has session
   *  context in scope. */
  sessionId?: string;
  /** Set to false to render the widget compact (inline) vs. card. */
  asCard?: boolean;
}

const TRAFFIC_LABEL: Record<string, string> = {
  green: '🟢 grün',
  yellow: '🟡 gelb',
  red: '🔴 rot',
};

const TRAFFIC_HINT =
  'Ampel: grün = 10–20 Quellen, ≥50 % wissenschaftlich, keine Blacklist-Treffer. ' +
  'Rot = Blacklist oder Anteil unter 50 %. Gelb = Mengen-/Aktualitätsproblem.';

const formatAuthors = (authors: StructuredReference['authors']) => {
  if (!authors || authors.length === 0) return 'o. A.';
  return authors
    .map(a => (a.given ? `${a.family}, ${a.given}` : a.family))
    .join('; ');
};

export const BibliographyWidget: React.FC<BibliographyWidgetProps> = ({
  draftId,
  sessionId,
  asCard = true,
}) => {
  const [entries, setEntries] = useState<StructuredReference[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [preview, setPreview] = useState<MigrationPreview | null>(null);
  const [portfolio, setPortfolio] = useState<PortfolioOutput | null>(null);
  const [portfolioLoading, setPortfolioLoading] = useState(false);

  const load = async () => {
    setLoading(true);
    setError(null);
    try {
      const refs = await getStructuredReferences(draftId);
      // Only show entries with a structured entry_key; legacy rows are
      // noise until the user runs the migration.
      setEntries(refs.filter(r => r.entry_key));
    } catch (e) {
      setError((e as Error).message || 'Failed to load references');
    } finally {
      setLoading(false);
    }
    // Pull the draft to read any pre-existing portfolio_output. We go
    // via the session-draft endpoint because the per-draft fetch is
    // only wired as part of WritingSessionWithDrafts today.
    if (sessionId) {
      try {
        const draft = await getSessionDraft(sessionId);
        setPortfolio(draft?.portfolio_output ?? null);
      } catch {
        // non-fatal — Portfolio stays collapsed.
      }
    }
  };

  useEffect(() => {
    if (draftId) void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [draftId, sessionId]);

  const onDelete = async (refId: string) => {
    try {
      await deleteStructuredReference(draftId, refId);
      setEntries(prev => prev.filter(e => e.id !== refId));
    } catch (e) {
      setError((e as Error).message || 'Delete failed');
    }
  };

  const onPreviewMigration = async () => {
    setLoading(true);
    setError(null);
    try {
      const pv = await previewBibliographyMigration(draftId);
      setPreview(pv);
    } catch (e) {
      setError((e as Error).message || 'Migration preview failed');
    } finally {
      setLoading(false);
    }
  };

  const onCommitMigration = async () => {
    setLoading(true);
    setError(null);
    try {
      await commitBibliographyMigration(draftId);
      setPreview(null);
      await load();
    } catch (e) {
      setError((e as Error).message || 'Migration commit failed');
    } finally {
      setLoading(false);
    }
  };

  const onGeneratePortfolio = async () => {
    setPortfolioLoading(true);
    setError(null);
    try {
      const output = await generateWritingPortfolio(draftId);
      setPortfolio(output);
    } catch (e) {
      setError((e as Error).message || 'Portfolio-Generierung fehlgeschlagen');
    } finally {
      setPortfolioLoading(false);
    }
  };

  const onRegeneratePortfolio = async () => {
    setPortfolioLoading(true);
    setError(null);
    try {
      await clearWritingPortfolio(draftId);
      const output = await generateWritingPortfolio(draftId);
      setPortfolio(output);
    } catch (e) {
      setError((e as Error).message || 'Portfolio-Neugenerierung fehlgeschlagen');
    } finally {
      setPortfolioLoading(false);
    }
  };

  const body = (
    <div className="space-y-3">
      {error && (
        <div className="flex items-center gap-2 text-sm text-destructive">
          <AlertCircle className="h-4 w-4" />
          <span>{error}</span>
        </div>
      )}

      {loading && (
        <p className="text-xs text-muted-foreground">Lade…</p>
      )}

      {!loading && entries.length === 0 && !preview && (
        <div className="space-y-2">
          <p className="text-sm text-muted-foreground">
            Noch keine strukturierten Einträge. Der Writing Agent erzeugt
            sie automatisch bei der nächsten Antwort mit Zitaten, oder du
            importierst ein bestehendes Markdown-Literaturverzeichnis.
          </p>
          <Button size="sm" variant="outline" onClick={onPreviewMigration}>
            <RefreshCw className="mr-2 h-3 w-3" />
            Aus Markdown importieren
          </Button>
        </div>
      )}

      {preview && (
        <div className="rounded border border-dashed p-3 space-y-2">
          <p className="text-sm font-medium">
            {preview.parsed_count} Einträge geparst
            {preview.unparsable_count > 0
              ? ` · ${preview.unparsable_count} unparsbar`
              : ''}
          </p>
          <ul className="text-xs space-y-1 max-h-40 overflow-auto">
            {preview.entries.map(e => (
              <li key={e.entry_key} className="truncate">
                <span className="font-mono">{e.entry_key}</span> — {e.title}
              </li>
            ))}
          </ul>
          {preview.unparsable_count > 0 && (
            <div className="text-xs text-amber-600">
              Unparsbare Zeilen: {preview.unparsable.slice(0, 3).join(' | ')}
              {preview.unparsable.length > 3 ? ' …' : ''}
            </div>
          )}
          <div className="flex gap-2">
            <Button size="sm" onClick={onCommitMigration} disabled={loading}>
              Übernehmen
            </Button>
            <Button
              size="sm"
              variant="ghost"
              onClick={() => setPreview(null)}
              disabled={loading}
            >
              Abbrechen
            </Button>
          </div>
        </div>
      )}

      {entries.length > 0 && (
        <ul className="space-y-1">
          {entries.map(ref => (
            <li
              key={ref.id}
              className="flex items-start justify-between gap-2 rounded border px-2 py-1 text-sm"
            >
              <div className="min-w-0 flex-1">
                <div className="truncate font-medium">
                  {ref.title ?? 'Untitled'}
                </div>
                <div className="truncate text-xs text-muted-foreground">
                  {formatAuthors(ref.authors)}
                  {ref.year ? ` · ${ref.year}` : ''}
                  {ref.container_title ? ` · ${ref.container_title}` : ''}
                  {ref.publisher ? ` · ${ref.publisher}` : ''}
                </div>
              </div>
              <Button
                size="icon"
                variant="ghost"
                className="h-7 w-7"
                onClick={() => onDelete(ref.id)}
                title="Löschen"
              >
                <Trash2 className="h-3.5 w-3.5" />
              </Button>
            </li>
          ))}
        </ul>
      )}

      {/* Literaturportfolio section (#61/#66) */}
      {entries.length > 0 && (
        <div className="mt-4 border-t pt-3">
          <div className="flex items-center justify-between gap-2 mb-2">
            <div className="flex items-center gap-2">
              <FileCheck2 className="h-4 w-4" />
              <span className="text-sm font-medium">Literaturportfolio</span>
              {portfolio?.compliance && (
                <span
                  className="text-xs px-2 py-0.5 rounded-full border"
                  title={TRAFFIC_HINT}
                >
                  {TRAFFIC_LABEL[portfolio.compliance.traffic_light] ?? portfolio.compliance.traffic_light}
                </span>
              )}
            </div>
            <div className="flex gap-1">
              {portfolio ? (
                <Button
                  size="sm"
                  variant="outline"
                  onClick={onRegeneratePortfolio}
                  disabled={portfolioLoading}
                  title="Portfolio mit aktuellem Bestand neu erzeugen"
                >
                  <RotateCcw className="mr-2 h-3 w-3" />
                  Aktualisieren
                </Button>
              ) : (
                <Button
                  size="sm"
                  onClick={onGeneratePortfolio}
                  disabled={portfolioLoading}
                  title="KMU-konformes Literaturportfolio erzeugen"
                >
                  <FileCheck2 className="mr-2 h-3 w-3" />
                  Portfolio generieren
                </Button>
              )}
            </div>
          </div>

          {portfolioLoading && (
            <p className="text-xs text-muted-foreground">
              Portfolio wird erzeugt … (Agent-Call, ca. 30 s)
            </p>
          )}

          {portfolio?.markdown_table && !portfolioLoading && (
            <div className="prose prose-sm max-w-none rounded border bg-muted/30 p-2 overflow-x-auto">
              <MathMarkdown content={portfolio.markdown_table} />
            </div>
          )}

          {!portfolio && !portfolioLoading && (
            <p className="text-xs text-muted-foreground">
              Noch kein Portfolio erzeugt. KMU verlangt die tabellarische
              Reflexionsleistung (Quellenangabe · Recherchetool · Relevanz ·
              Qualität) mit jedem Einsendeschein.
            </p>
          )}
        </div>
      )}
    </div>
  );

  if (!asCard) return body;

  return (
    <Card>
      <CardHeader className="pb-2">
        <CardTitle className="flex items-center gap-2 text-sm">
          <BookOpen className="h-4 w-4" />
          Strukturiertes Literaturverzeichnis
          {entries.length > 0 && (
            <span className="text-xs font-normal text-muted-foreground">
              ({entries.length})
            </span>
          )}
        </CardTitle>
      </CardHeader>
      <CardContent>{body}</CardContent>
    </Card>
  );
};
