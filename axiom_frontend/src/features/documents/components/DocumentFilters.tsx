import React, { useState, useEffect, useCallback, useMemo } from 'react';
import { Search, Filter, X, ChevronDown, Calendar, User, BookOpen, FileType, Zap } from 'lucide-react';
import { getFilterOptions, fulltextSearchDocuments, type FulltextSearchResult } from '../api';
import { useDebounce } from '../../../hooks/useDebounce';

const DOCUMENT_TYPE_LABELS: Record<string, string> = {
  academic: 'Academic Paper',
  book: 'Book',
  legal: 'Legal/Regulatory',
  institutional: 'Institutional Report',
  web: 'Web Source',
  wikipedia: 'Wikipedia',
};

interface FilterOptions {
  search: string;
  author: string;
  year: string;
  journal: string;
  documentType: string;
}

interface DocumentFiltersProps {
  filters: FilterOptions;
  onFiltersChange: (filters: FilterOptions) => void;
  groupId?: string;
  documentCount?: number;
  onFulltextResults?: (results: FulltextSearchResult[] | null) => void;
}

export const DocumentFilters: React.FC<DocumentFiltersProps> = ({
  filters,
  onFiltersChange,
  groupId,
  documentCount = 0,
  onFulltextResults
}) => {
  const [showAdvancedFilters, setShowAdvancedFilters] = useState(false);
  const [filterOptions, setFilterOptions] = useState<{
    authors: string[];
    years: number[];
    journals: string[];
    documentTypes: string[];
  }>({ authors: [], years: [], journals: [], documentTypes: [] });

  const [localSearch, setLocalSearch] = useState(filters.search);
  const [deepSearchEnabled, setDeepSearchEnabled] = useState(false);
  const [isDeepSearching, setIsDeepSearching] = useState(false);
  const [dropdownOpen, setDropdownOpen] = useState<string | null>(null);
  const [searchTerms, setSearchTerms] = useState<{ [key: string]: string }>({
    author: '',
    year: '',
    journal: '',
    documentType: ''
  });
  
  // Debounce search input
  const debouncedSearch = useDebounce(localSearch, 500);
  
  // Load filter options
  useEffect(() => {
    const loadFilterOptions = async () => {
      try {
        const options = await getFilterOptions(groupId);
        setFilterOptions({
          authors: options.authors || [],
          years: options.years || [],
          journals: options.journals || [],
          documentTypes: options.document_types || [],
        });
      } catch (error) {
        console.error('Failed to load filter options:', error);
      }
    };
    loadFilterOptions();
  }, [groupId]);
  
  // Update filters when debounced search changes
  useEffect(() => {
    if (debouncedSearch !== filters.search) {
      onFiltersChange({ ...filters, search: debouncedSearch });
    }
  }, [debouncedSearch]);

  // Deep search using OpenSearch fulltext
  useEffect(() => {
    if (!deepSearchEnabled || !onFulltextResults) return;
    if (!debouncedSearch || debouncedSearch.length < 3) {
      onFulltextResults(null);
      return;
    }
    let cancelled = false;
    const runSearch = async () => {
      setIsDeepSearching(true);
      try {
        const results = await fulltextSearchDocuments(debouncedSearch, 20, groupId);
        if (!cancelled) {
          onFulltextResults(results);
        }
      } catch (err) {
        console.error('Deep search failed:', err);
        if (!cancelled) {
          onFulltextResults(null);
        }
      } finally {
        if (!cancelled) setIsDeepSearching(false);
      }
    };
    runSearch();
    return () => { cancelled = true; };
  }, [debouncedSearch, deepSearchEnabled, groupId, onFulltextResults]);
  
  const handleSearchChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    setLocalSearch(e.target.value);
  };
  
  const handleFilterChange = (key: keyof FilterOptions, value: string) => {
    onFiltersChange({ ...filters, [key]: value });
    setDropdownOpen(null);
  };
  
  const clearFilter = (key: keyof FilterOptions) => {
    onFiltersChange({ ...filters, [key]: '' });
  };
  
  const clearAllFilters = () => {
    setLocalSearch('');
    onFiltersChange({
      search: '',
      author: '',
      year: '',
      journal: '',
      documentType: ''
    });
    if (onFulltextResults) onFulltextResults(null);
  };

  const hasActiveFilters = useMemo(() => {
    return filters.author || filters.year || filters.journal || filters.documentType;
  }, [filters]);

  const activeFilterCount = useMemo(() => {
    let count = 0;
    if (filters.author) count++;
    if (filters.year) count++;
    if (filters.journal) count++;
    if (filters.documentType) count++;
    return count;
  }, [filters]);
  
  const toggleDropdown = (dropdown: string) => {
    setDropdownOpen(dropdownOpen === dropdown ? null : dropdown);
    // Reset search term when opening dropdown
    setSearchTerms(prev => ({ ...prev, [dropdown]: '' }));
  };
  
  const handleSearchTermChange = (field: string, value: string) => {
    setSearchTerms(prev => ({ ...prev, [field]: value }));
  };
  
  const getFilteredOptions = (field: string, options: any[]) => {
    const searchTerm = searchTerms[field]?.toLowerCase() || '';
    if (!searchTerm) return options;
    
    return options.filter(option => {
      const optionStr = option.toString().toLowerCase();
      return optionStr.includes(searchTerm);
    });
  };
  
  return (
    <div className="bg-background border-b border-border">
      {/* Search Bar and Filter Toggle on Same Row */}
      <div className="p-4 pb-2 flex items-center gap-3">
        <div className="flex-1 relative">
          <Search className="absolute left-3 top-1/2 transform -translate-y-1/2 h-4 w-4 text-muted-foreground" />
          <input
            type="text"
            placeholder="Search by title, author, or filename..."
            value={localSearch}
            onChange={handleSearchChange}
            className="w-full pl-10 pr-4 py-2 border border-border rounded-lg focus:ring-2 focus:ring-primary focus:border-transparent text-sm bg-background text-foreground placeholder:text-muted-foreground"
          />
          {localSearch && (
            <button
              onClick={() => setLocalSearch('')}
              className="absolute right-3 top-1/2 transform -translate-y-1/2 text-muted-foreground hover:text-foreground"
            >
              <X className="h-4 w-4" />
            </button>
          )}
        </div>

        {/* Deep Search Toggle */}
        {onFulltextResults && (
          <button
            onClick={() => setDeepSearchEnabled(!deepSearchEnabled)}
            className={`flex items-center gap-1.5 px-3 py-2 text-sm rounded-lg transition-colors border whitespace-nowrap ${
              deepSearchEnabled
                ? 'bg-amber-500/10 text-amber-600 border-amber-500/30 hover:bg-amber-500/20'
                : 'bg-muted text-muted-foreground hover:bg-muted/80 border-border'
            }`}
            title="Deep search uses full-text content search for more thorough results"
          >
            <Zap className={`h-4 w-4 ${isDeepSearching ? 'animate-pulse' : ''}`} />
            <span>Deep</span>
          </button>
        )}

        {/* Filter Toggle Button */}
        <button
          onClick={() => setShowAdvancedFilters(!showAdvancedFilters)}
          className={`flex items-center gap-2 px-4 py-2 text-sm rounded-lg transition-colors border ${
            showAdvancedFilters || hasActiveFilters
              ? 'bg-primary text-primary-foreground border-primary'
              : 'bg-muted text-foreground hover:bg-muted/80 border-border'
          }`}
        >
          <Filter className="h-4 w-4" />
          <span>Filters</span>
          {activeFilterCount > 0 && (
            <span className="ml-1 px-1.5 py-0.5 text-xs bg-background text-foreground rounded-full">
              {activeFilterCount}
            </span>
          )}
          <ChevronDown className={`h-4 w-4 transition-transform ${
            showAdvancedFilters ? 'rotate-180' : ''
          }`} />
        </button>
      </div>
      
      {/* Document Count and Active Filter Chips on Second Row */}
      <div className="px-4 pb-3 flex items-center justify-between">
        <div className="text-sm text-muted-foreground">
          {documentCount} documents
        </div>
        
        {hasActiveFilters && (
          <div className="flex items-center gap-2">
            {filters.author && (
              <div className="flex items-center gap-1 px-2 py-1 bg-primary/10 text-primary rounded-md text-xs">
                <User className="h-3 w-3" />
                <span className="max-w-[150px] truncate">{filters.author}</span>
                <button
                  onClick={() => clearFilter('author')}
                  className="ml-1 hover:text-primary/80"
                >
                  <X className="h-3 w-3" />
                </button>
              </div>
            )}
            {filters.year && (
              <div className="flex items-center gap-1 px-2 py-1 bg-primary/10 text-primary rounded-md text-xs">
                <Calendar className="h-3 w-3" />
                <span>{filters.year}</span>
                <button
                  onClick={() => clearFilter('year')}
                  className="ml-1 hover:text-primary/80"
                >
                  <X className="h-3 w-3" />
                </button>
              </div>
            )}
            {filters.journal && (
              <div className="flex items-center gap-1 px-2 py-1 bg-primary/10 text-primary rounded-md text-xs">
                <BookOpen className="h-3 w-3" />
                <span className="max-w-[150px] truncate">{filters.journal}</span>
                <button
                  onClick={() => clearFilter('journal')}
                  className="ml-1 hover:text-primary/80"
                >
                  <X className="h-3 w-3" />
                </button>
              </div>
            )}
            {filters.documentType && (
              <div className="flex items-center gap-1 px-2 py-1 bg-primary/10 text-primary rounded-md text-xs">
                <FileType className="h-3 w-3" />
                <span>{DOCUMENT_TYPE_LABELS[filters.documentType] || filters.documentType}</span>
                <button
                  onClick={() => clearFilter('documentType')}
                  className="ml-1 hover:text-primary/80"
                >
                  <X className="h-3 w-3" />
                </button>
              </div>
            )}

            <button
              onClick={clearAllFilters}
              className="text-xs text-muted-foreground hover:text-foreground ml-2"
            >
              Clear all
            </button>
          </div>
        )}
      </div>
      
      {/* Advanced Filters Dropdown */}
      {showAdvancedFilters && (
        <div className="px-4 pb-4">
          <div className="p-4 bg-muted/30 rounded-lg border border-border">
            <div className="grid grid-cols-1 md:grid-cols-4 gap-4">
              {/* Author Filter */}
              <div className="relative">
                <label className="block text-xs font-medium text-foreground mb-1.5">
                  Author
                </label>
                <div className="relative">
                  <button
                    onClick={() => toggleDropdown('author')}
                    className="w-full px-3 py-2 text-left border border-border rounded-md bg-background hover:bg-muted/50 focus:ring-2 focus:ring-primary focus:border-transparent flex items-center justify-between text-sm"
                  >
                    <span className={filters.author ? 'text-foreground' : 'text-muted-foreground'}>
                      {filters.author || 'Select author...'}
                    </span>
                    <ChevronDown className="h-4 w-4 text-muted-foreground" />
                  </button>
                  
                  {dropdownOpen === 'author' && (
                    <div className="absolute z-50 mt-1 w-full bg-background border border-border rounded-md shadow-lg max-h-60 overflow-hidden flex flex-col">
                      <div className="sticky top-0 bg-background border-b border-border p-2">
                        <input
                          type="text"
                          placeholder="Type to search..."
                          value={searchTerms.author}
                          onChange={(e) => handleSearchTermChange('author', e.target.value)}
                          onClick={(e) => e.stopPropagation()}
                          className="w-full px-2 py-1 text-sm border border-border rounded bg-background focus:ring-1 focus:ring-primary focus:border-primary"
                          autoFocus
                        />
                      </div>
                      <div className="overflow-auto max-h-48">
                      <button
                        onClick={() => handleFilterChange('author', '')}
                        className="w-full px-3 py-2 text-left text-sm hover:bg-muted/50 text-muted-foreground"
                      >
                        All authors
                      </button>
                      {getFilteredOptions('author', filterOptions.authors).length === 0 ? (
                        <div className="px-3 py-2 text-sm text-muted-foreground italic">
                          No matching authors found
                        </div>
                      ) : (
                        getFilteredOptions('author', filterOptions.authors).map(author => (
                        <button
                          key={author}
                          onClick={() => handleFilterChange('author', author)}
                          className={`w-full px-3 py-2 text-left text-sm hover:bg-muted/50 ${
                            filters.author === author ? 'bg-primary/10 text-primary' : 'text-foreground'
                          }`}
                        >
                          {author}
                        </button>
                        ))
                      )}
                      </div>
                    </div>
                  )}
                </div>
              </div>
              
              {/* Year Filter */}
              <div className="relative">
                <label className="block text-xs font-medium text-foreground mb-1.5">
                  Year
                </label>
                <div className="relative">
                  <button
                    onClick={() => toggleDropdown('year')}
                    className="w-full px-3 py-2 text-left border border-border rounded-md bg-background hover:bg-muted/50 focus:ring-2 focus:ring-primary focus:border-transparent flex items-center justify-between text-sm"
                  >
                    <span className={filters.year ? 'text-foreground' : 'text-muted-foreground'}>
                      {filters.year || 'Select year...'}
                    </span>
                    <ChevronDown className="h-4 w-4 text-muted-foreground" />
                  </button>
                  
                  {dropdownOpen === 'year' && (
                    <div className="absolute z-50 mt-1 w-full bg-background border border-border rounded-md shadow-lg max-h-60 overflow-hidden flex flex-col">
                      <div className="sticky top-0 bg-background border-b border-border p-2">
                        <input
                          type="text"
                          placeholder="Type year..."
                          value={searchTerms.year}
                          onChange={(e) => handleSearchTermChange('year', e.target.value)}
                          onClick={(e) => e.stopPropagation()}
                          className="w-full px-2 py-1 text-sm border border-border rounded bg-background focus:ring-1 focus:ring-primary focus:border-primary"
                          autoFocus
                        />
                      </div>
                      <div className="overflow-auto max-h-48">
                      <button
                        onClick={() => handleFilterChange('year', '')}
                        className="w-full px-3 py-2 text-left text-sm hover:bg-muted/50 text-muted-foreground"
                      >
                        All years
                      </button>
                      {getFilteredOptions('year', filterOptions.years).length === 0 ? (
                        <div className="px-3 py-2 text-sm text-muted-foreground italic">
                          No matching years found
                        </div>
                      ) : (
                        getFilteredOptions('year', filterOptions.years).map(year => (
                        <button
                          key={year}
                          onClick={() => handleFilterChange('year', year.toString())}
                          className={`w-full px-3 py-2 text-left text-sm hover:bg-muted/50 ${
                            filters.year === year.toString() ? 'bg-primary/10 text-primary' : 'text-foreground'
                          }`}
                        >
                          {year}
                        </button>
                        ))
                      )}
                      </div>
                    </div>
                  )}
                </div>
              </div>
              
              {/* Journal Filter */}
              <div className="relative">
                <label className="block text-xs font-medium text-foreground mb-1.5">
                  Journal
                </label>
                <div className="relative">
                  <button
                    onClick={() => toggleDropdown('journal')}
                    className="w-full px-3 py-2 text-left border border-border rounded-md bg-background hover:bg-muted/50 focus:ring-2 focus:ring-primary focus:border-transparent flex items-center justify-between text-sm"
                  >
                    <span className={filters.journal ? 'text-foreground' : 'text-muted-foreground'}>
                      {filters.journal || 'Select journal...'}
                    </span>
                    <ChevronDown className="h-4 w-4 text-muted-foreground" />
                  </button>
                  
                  {dropdownOpen === 'journal' && (
                    <div className="absolute z-50 mt-1 w-full bg-background border border-border rounded-md shadow-lg max-h-60 overflow-hidden flex flex-col">
                      <div className="sticky top-0 bg-background border-b border-border p-2">
                        <input
                          type="text"
                          placeholder="Type to search..."
                          value={searchTerms.journal}
                          onChange={(e) => handleSearchTermChange('journal', e.target.value)}
                          onClick={(e) => e.stopPropagation()}
                          className="w-full px-2 py-1 text-sm border border-border rounded bg-background focus:ring-1 focus:ring-primary focus:border-primary"
                          autoFocus
                        />
                      </div>
                      <div className="overflow-auto max-h-48">
                      <button
                        onClick={() => handleFilterChange('journal', '')}
                        className="w-full px-3 py-2 text-left text-sm hover:bg-muted/50 text-muted-foreground"
                      >
                        All journals
                      </button>
                      {getFilteredOptions('journal', filterOptions.journals).length === 0 ? (
                        <div className="px-3 py-2 text-sm text-muted-foreground italic">
                          No matching journals found
                        </div>
                      ) : (
                        getFilteredOptions('journal', filterOptions.journals).map(journal => (
                        <button
                          key={journal}
                          onClick={() => handleFilterChange('journal', journal)}
                          className={`w-full px-3 py-2 text-left text-sm hover:bg-muted/50 ${
                            filters.journal === journal ? 'bg-primary/10 text-primary' : 'text-foreground'
                          }`}
                        >
                          {journal}
                        </button>
                        ))
                      )}
                      </div>
                    </div>
                  )}
                </div>
              </div>

              {/* Document Type Filter */}
              <div className="relative">
                <label className="block text-xs font-medium text-foreground mb-1.5">
                  Document Type
                </label>
                <div className="relative">
                  <button
                    onClick={() => toggleDropdown('documentType')}
                    className="w-full px-3 py-2 text-left border border-border rounded-md bg-background hover:bg-muted/50 focus:ring-2 focus:ring-primary focus:border-transparent flex items-center justify-between text-sm"
                  >
                    <span className={filters.documentType ? 'text-foreground' : 'text-muted-foreground'}>
                      {filters.documentType ? (DOCUMENT_TYPE_LABELS[filters.documentType] || filters.documentType) : 'Select type...'}
                    </span>
                    <ChevronDown className="h-4 w-4 text-muted-foreground" />
                  </button>

                  {dropdownOpen === 'documentType' && (
                    <div className="absolute z-50 mt-1 w-full bg-background border border-border rounded-md shadow-lg max-h-60 overflow-hidden flex flex-col">
                      <div className="overflow-auto max-h-48">
                      <button
                        onClick={() => handleFilterChange('documentType', '')}
                        className="w-full px-3 py-2 text-left text-sm hover:bg-muted/50 text-muted-foreground"
                      >
                        All types
                      </button>
                      {filterOptions.documentTypes.length === 0 ? (
                        <div className="px-3 py-2 text-sm text-muted-foreground italic">
                          No document types found
                        </div>
                      ) : (
                        filterOptions.documentTypes.map(docType => (
                        <button
                          key={docType}
                          onClick={() => handleFilterChange('documentType', docType)}
                          className={`w-full px-3 py-2 text-left text-sm hover:bg-muted/50 ${
                            filters.documentType === docType ? 'bg-primary/10 text-primary' : 'text-foreground'
                          }`}
                        >
                          {DOCUMENT_TYPE_LABELS[docType] || docType}
                        </button>
                        ))
                      )}
                      </div>
                    </div>
                  )}
                </div>
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  );
};