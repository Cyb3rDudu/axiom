import type { Document } from '../types';

export interface MetadataCompletenessResult {
  score: number;
  missingFields: string[];
  level: 'complete' | 'partial' | 'poor';
  /** Set when the document is Wikipedia — signals special UI treatment. */
  isWikipedia?: boolean;
}

type DocumentType = 'academic' | 'book' | 'legal' | 'institutional' | 'web' | 'wikipedia';

/**
 * Classify a document into a type based on its metadata and filename.
 * Mirrors the backend `classify_document_type()` logic.
 */
export function classifyDocumentType(metadata: Record<string, unknown>, filename = ''): DocumentType {
  const title = ((metadata.title as string) || '').toLowerCase();
  const url = ((metadata.url as string) || '').toLowerCase();
  const fn = filename.toLowerCase();
  const authors = metadata.authors;
  const doi = metadata.doi;
  const isbn = metadata.isbn;

  // Wikipedia detection
  if (url.includes('wikipedia.org') || title.includes('wikipedia')) {
    return 'wikipedia';
  }

  // Legal/regulatory detection (German law references)
  const legalPatterns = ['§', 'sgb', 'ksvg', 'aktg', 'bgb', 'hgb', 'gesetz', 'verordnung',
    'richtlinie', 'rechtsverordnung', 'satzung'];
  if (legalPatterns.some(p => title.includes(p)) || legalPatterns.some(p => fn.includes(p))) {
    return 'legal';
  }

  // Book detection
  if (isbn || metadata.publisher || metadata.edition || metadata.chapters) {
    return 'book';
  }

  // Academic detection
  if (doi || metadata.journal_or_source) {
    return 'academic';
  }
  if (fn.endsWith('.pdf') && Array.isArray(authors) && authors.length > 0) {
    return 'academic';
  }

  // Institutional reports
  const instPatterns = ['ezb', 'ecb', 'bundesbank', 'euroraum', 'projektion', 'prognose',
    'gemeinschaftsdiagnose', 'sachverständigenrat', 'bundesregierung',
    'bundesministerium', 'european commission', 'imf', 'world bank', 'oecd'];
  if (instPatterns.some(p => title.includes(p)) || metadata.organization) {
    return 'institutional';
  }

  // Web documents
  if (fn.includes('_web_document') || url) {
    return 'web';
  }

  // Default
  if (fn.endsWith('.pdf') || fn.endsWith('.docx')) {
    return 'academic';
  }
  return 'web';
}

/**
 * Check if authors field is meaningfully populated.
 */
function hasAuthors(authors: unknown): boolean {
  if (!authors) return false;
  if (Array.isArray(authors)) return authors.length > 0;
  if (typeof authors === 'string') return authors !== '' && authors !== '[]';
  return false;
}

/**
 * Calculate metadata completeness for a document.
 * Returns a score (0-100), list of missing fields, and a level classification.
 *
 * If the API provides `metadata_completeness` on the metadata object, that
 * value is used directly. Otherwise the score is computed client-side from
 * the fields present in `metadata_`.
 */
export function calculateMetadataCompleteness(doc: Document): MetadataCompletenessResult {
  const metadata = doc.metadata_;

  if (!metadata) {
    return { score: 0, missingFields: ['title', 'authors', 'publication year', 'DOI/ISBN', 'journal/source', 'abstract'], level: 'poor' };
  }

  // If the API already computed a score, use it (but still derive missing fields + type).
  const apiScore = metadata.metadata_completeness as number | undefined;
  const filename = doc.original_filename || '';
  const docType: DocumentType = (metadata.document_type as DocumentType) || classifyDocumentType(metadata as Record<string, unknown>, filename);

  // Wikipedia: always poor, special flag
  if (docType === 'wikipedia') {
    return {
      score: apiScore ?? 0,
      missingFields: [],
      level: 'poor',
      isWikipedia: true,
    };
  }

  const missingFields: string[] = [];
  let score = 0;

  if (docType === 'academic') {
    if (metadata.title) { score += 25; } else { missingFields.push('title'); }
    if (hasAuthors(metadata.authors)) { score += 25; } else { missingFields.push('authors'); }
    if (metadata.publication_year) { score += 20; } else { missingFields.push('publication year'); }
    if (metadata.doi || metadata.isbn) { score += 10; } else { missingFields.push('DOI/ISBN'); }
    if (metadata.journal_or_source) { score += 10; } else { missingFields.push('journal/source'); }
    if (metadata.description || metadata.abstract) { score += 10; } else { missingFields.push('abstract'); }
  } else if (docType === 'book') {
    if (metadata.title) { score += 25; } else { missingFields.push('title'); }
    if (hasAuthors(metadata.authors)) { score += 25; } else { missingFields.push('authors'); }
    if (metadata.publication_year) { score += 20; } else { missingFields.push('publication year'); }
    if (metadata.isbn) { score += 15; } else { missingFields.push('ISBN'); }
    if (metadata.publisher) { score += 15; } else { missingFields.push('publisher'); }
  } else if (docType === 'legal') {
    const title = (metadata.title as string) || '';
    if (title) { score += 40; } else { missingFields.push('title'); }
    if (title.includes('§')) { score += 30; } else { missingFields.push('section reference (§)'); }
    if (metadata.url) { score += 15; } else { missingFields.push('URL'); }
    if (metadata.publication_year) { score += 15; } else { missingFields.push('effective date'); }
    score = Math.min(score, 100);
  } else if (docType === 'institutional') {
    if (metadata.title) { score += 25; } else { missingFields.push('title'); }
    const org = metadata.organization || metadata.website_name;
    if (org) { score += 25; } else if (hasAuthors(metadata.authors)) { score += 25; } else { missingFields.push('organization'); }
    if (metadata.publication_year) { score += 20; } else { missingFields.push('publication year'); }
    if (metadata.url) { score += 20; } else { missingFields.push('URL'); }
    if (metadata.description || metadata.abstract) { score += 10; } else { missingFields.push('description'); }
    score = Math.min(score, 100);
  } else {
    // 'web' and other
    if (metadata.title) { score += 25; } else { missingFields.push('title'); }
    const site = metadata.website_name || metadata.organization;
    if (hasAuthors(metadata.authors) || site) { score += 25; } else { missingFields.push('author/site name'); }
    if (metadata.publication_year) { score += 20; } else { missingFields.push('publication year'); }
    if (metadata.url) { score += 20; } else { missingFields.push('URL'); }
    if (metadata.description) { score += 10; } else { missingFields.push('description'); }
    score = Math.min(score, 100);
  }

  const finalScore = apiScore ?? score;

  const level: MetadataCompletenessResult['level'] =
    finalScore >= 80 ? 'complete' : finalScore >= 40 ? 'partial' : 'poor';

  return { score: finalScore, missingFields, level };
}

/**
 * Summarise metadata completeness across a list of documents.
 * Returns the number of documents with incomplete metadata (score < 80).
 */
export function countIncompleteDocuments(documents: Document[]): {
  incomplete: number;
  total: number;
} {
  let incomplete = 0;
  for (const doc of documents) {
    if (calculateMetadataCompleteness(doc).score < 80) {
      incomplete++;
    }
  }
  return { incomplete, total: documents.length };
}
