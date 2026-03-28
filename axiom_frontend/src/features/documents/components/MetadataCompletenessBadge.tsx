import React, { useState, useRef, useEffect } from 'react';
import { CheckCircle, AlertTriangle, AlertCircle } from 'lucide-react';
import type { Document } from '../types';
import { calculateMetadataCompleteness } from '../utils/metadataCompleteness';

interface MetadataCompletenessBadgeProps {
  document: Document;
  /** Called when the user clicks "Edit metadata" inside the tooltip. */
  onEditMetadata?: (doc: Document) => void;
}

/**
 * Small inline badge that indicates metadata completeness for a document.
 * - Green checkmark: >= 80  (complete)
 * - Amber warning:  40-79   (partial)
 * - Red alert:      < 40    (poor)
 *
 * On hover (for incomplete documents) a tooltip shows which fields are missing
 * and provides a link to open the metadata editor.
 */
export const MetadataCompletenessBadge: React.FC<MetadataCompletenessBadgeProps> = ({
  document,
  onEditMetadata,
}) => {
  const { score, missingFields, level } = calculateMetadataCompleteness(document);
  const [showTooltip, setShowTooltip] = useState(false);
  const tooltipRef = useRef<HTMLDivElement>(null);
  const badgeRef = useRef<HTMLDivElement>(null);

  // Close tooltip when clicking outside
  useEffect(() => {
    if (!showTooltip) return;
    const handleClickOutside = (e: MouseEvent) => {
      if (
        tooltipRef.current &&
        !tooltipRef.current.contains(e.target as Node) &&
        badgeRef.current &&
        !badgeRef.current.contains(e.target as Node)
      ) {
        setShowTooltip(false);
      }
    };
    window.addEventListener('mousedown', handleClickOutside);
    return () => window.removeEventListener('mousedown', handleClickOutside);
  }, [showTooltip]);

  const iconClasses = 'h-3.5 w-3.5 flex-shrink-0';

  const icon =
    level === 'complete' ? (
      <CheckCircle className={`${iconClasses} text-emerald-500`} />
    ) : level === 'partial' ? (
      <AlertTriangle className={`${iconClasses} text-amber-500`} />
    ) : (
      <AlertCircle className={`${iconClasses} text-red-500`} />
    );

  // Complete documents get a simple icon, no tooltip needed.
  if (level === 'complete') {
    return (
      <div
        className="flex items-center"
        title={`Metadata completeness: ${score}%`}
      >
        {icon}
      </div>
    );
  }

  return (
    <div className="relative inline-flex" ref={badgeRef}>
      <button
        type="button"
        className="flex items-center focus:outline-none"
        onMouseEnter={() => setShowTooltip(true)}
        onMouseLeave={() => setShowTooltip(false)}
        onClick={(e) => {
          e.stopPropagation();
          setShowTooltip((prev) => !prev);
        }}
        aria-label={`Metadata ${level}: ${score}% complete`}
      >
        {icon}
      </button>

      {showTooltip && (
        <div
          ref={tooltipRef}
          className="fixed z-[9999] w-56 rounded-lg border border-border bg-popover p-3 text-popover-foreground shadow-md"
          style={{
            top: badgeRef.current ? badgeRef.current.getBoundingClientRect().top - 8 : 0,
            left: badgeRef.current ? badgeRef.current.getBoundingClientRect().right + 8 : 0,
            transform: 'translateY(-100%)',
          }}
          onMouseEnter={() => setShowTooltip(true)}
          onMouseLeave={() => setShowTooltip(false)}
        >
          <p className="text-xs font-medium mb-1.5">
            Incomplete metadata ({score}%)
          </p>
          <p className="text-xs text-muted-foreground mb-2">
            Citations may show placeholder text.
          </p>

          {missingFields.length > 0 && (
            <div className="mb-2">
              <p className="text-xs text-muted-foreground mb-1">Missing:</p>
              <ul className="text-xs text-muted-foreground list-disc list-inside space-y-0.5">
                {missingFields.map((field) => (
                  <li key={field}>{field}</li>
                ))}
              </ul>
            </div>
          )}

          {onEditMetadata && (
            <button
              type="button"
              className="text-xs text-primary hover:underline"
              onClick={(e) => {
                e.stopPropagation();
                setShowTooltip(false);
                onEditMetadata(document);
              }}
            >
              Click to edit metadata
            </button>
          )}
        </div>
      )}
    </div>
  );
};
