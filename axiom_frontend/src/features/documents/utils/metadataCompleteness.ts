import type { Document } from '../types';

export interface MetadataCompletenessResult {
  score: number;
  missingFields: string[];
  level: 'complete' | 'partial' | 'poor';
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

  // If the API already computed a score, use it (but still derive missing fields).
  const apiScore = metadata?.metadata_completeness as number | undefined;

  const missingFields: string[] = [];
  let score = 0;

  if (!metadata) {
    return { score: apiScore ?? 0, missingFields: ['title', 'authors', 'publication year', 'DOI/ISBN', 'journal/source', 'abstract'], level: 'poor' };
  }

  // Title (25 pts)
  if (metadata.title) {
    score += 25;
  } else {
    missingFields.push('title');
  }

  // Authors (25 pts)
  const authors = metadata.authors;
  if (authors && (Array.isArray(authors) ? authors.length > 0 : authors !== '')) {
    score += 25;
  } else {
    missingFields.push('authors');
  }

  // Publication year (20 pts)
  if (metadata.publication_year) {
    score += 20;
  } else {
    missingFields.push('publication year');
  }

  // DOI or ISBN (10 pts)
  if (metadata.doi || metadata.isbn) {
    score += 10;
  } else {
    missingFields.push('DOI/ISBN');
  }

  // Journal / source (10 pts)
  if (metadata.journal_or_source) {
    score += 10;
  } else {
    missingFields.push('journal/source');
  }

  // Abstract / description (10 pts)
  if (metadata.abstract || metadata.description) {
    score += 10;
  } else {
    missingFields.push('abstract');
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
