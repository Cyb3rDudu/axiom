import React, { useMemo } from 'react'
import { Check, ChevronDown, Folder } from 'lucide-react'
import { cn } from '../../lib/utils'
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
} from './dropdown-menu'

/**
 * Lightweight multi-select for document groups.
 *
 * Deliberately thin: no virtual list, no search box. The doc-group set a
 * single user has is small (usually < 10), and axiom's existing
 * DropdownMenu already handles outside-click and keyboard. The trigger
 * collapses long selections into "VWL + 1 more" / "3 Gruppen" so the
 * chat toolbar stays compact.
 */
export interface DocGroupOption {
  id: string
  name: string
}

interface MultiGroupSelectProps {
  groups: DocGroupOption[]
  selectedIds: string[]
  onChange: (next: string[]) => void
  placeholder?: string
  className?: string
  disabled?: boolean
  /** Whether an empty selection is allowed. When false, unchecking the
   *  last group is a no-op. */
  allowEmpty?: boolean
}

export const MultiGroupSelect: React.FC<MultiGroupSelectProps> = ({
  groups,
  selectedIds,
  onChange,
  placeholder = 'Gruppen wählen',
  className,
  disabled = false,
  allowEmpty = true,
}) => {
  const selectedSet = useMemo(() => new Set(selectedIds), [selectedIds])
  const selectedNames = useMemo(
    () => groups.filter(g => selectedSet.has(g.id)).map(g => g.name),
    [groups, selectedSet]
  )

  const triggerLabel = useMemo(() => {
    if (selectedNames.length === 0) return placeholder
    if (selectedNames.length === 1) return selectedNames[0]
    if (selectedNames.length === 2) return selectedNames.join(' + ')
    return `${selectedNames.length} Gruppen`
  }, [selectedNames, placeholder])

  const toggle = (id: string) => {
    if (selectedSet.has(id)) {
      if (!allowEmpty && selectedIds.length === 1) return
      onChange(selectedIds.filter(x => x !== id))
    } else {
      onChange([...selectedIds, id])
    }
  }

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <button
          type="button"
          disabled={disabled}
          className={cn(
            'inline-flex items-center gap-1.5 rounded-md border border-border',
            'bg-background px-2.5 py-1.5 text-xs text-foreground',
            'hover:bg-muted disabled:opacity-50 disabled:cursor-not-allowed',
            'max-w-[220px] truncate',
            className
          )}
          title={selectedNames.length > 0 ? selectedNames.join(', ') : placeholder}
        >
          <Folder className="h-3 w-3 shrink-0 text-text-secondary" />
          <span className="truncate">{triggerLabel}</span>
          <ChevronDown className="h-3 w-3 shrink-0 text-text-secondary" />
        </button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="min-w-[240px] max-h-[320px] overflow-y-auto">
        {groups.length === 0 ? (
          <div className="px-3 py-2 text-xs text-text-secondary">
            Keine Gruppen verfügbar
          </div>
        ) : (
          <>
            {groups.map(group => {
              const checked = selectedSet.has(group.id)
              return (
                <DropdownMenuItem
                  key={group.id}
                  onClick={e => {
                    // Don't close the dropdown when toggling — users
                    // typically want to flip several in one go.
                    e.preventDefault()
                    e.stopPropagation()
                    toggle(group.id)
                  }}
                  className="flex items-center gap-2 cursor-pointer"
                >
                  <div
                    className={cn(
                      'flex h-4 w-4 shrink-0 items-center justify-center rounded border',
                      checked
                        ? 'bg-primary border-primary text-primary-foreground'
                        : 'border-border bg-background'
                    )}
                  >
                    {checked && <Check className="h-3 w-3" />}
                  </div>
                  <span className="flex-1 truncate text-sm">{group.name}</span>
                </DropdownMenuItem>
              )
            })}
            {selectedIds.length > 0 && allowEmpty && (
              <DropdownMenuItem
                onClick={e => {
                  e.preventDefault()
                  e.stopPropagation()
                  onChange([])
                }}
                className="border-t border-border mt-1 pt-2 text-xs text-text-secondary"
              >
                Auswahl leeren
              </DropdownMenuItem>
            )}
          </>
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
