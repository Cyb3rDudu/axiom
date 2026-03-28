import React from 'react';
import { AlertTriangle } from 'lucide-react';
import { Alert, AlertDescription } from '../../../components/ui/alert';
import type { Document } from '../types';
import { countIncompleteDocuments } from '../utils/metadataCompleteness';

interface MetadataWarningBannerProps {
  documents: Document[];
}

/**
 * A warning banner displayed when a document group contains documents
 * with incomplete metadata (completeness < 80%). Hidden when all
 * documents are sufficiently complete.
 */
export const MetadataWarningBanner: React.FC<MetadataWarningBannerProps> = ({
  documents,
}) => {
  if (documents.length === 0) return null;

  const { incomplete, total } = countIncompleteDocuments(documents);

  if (incomplete === 0) return null;

  return (
    <Alert className="mx-4 mt-3 border-amber-500/40 bg-amber-500/10">
      <AlertTriangle className="h-4 w-4 text-amber-500" />
      <AlertDescription className="text-xs text-foreground ml-2">
        <span className="font-medium">{incomplete} of {total}</span>{' '}
        document{incomplete !== 1 ? 's have' : ' has'} incomplete metadata.
        Citations in research reports may be inaccurate.
      </AlertDescription>
    </Alert>
  );
};
