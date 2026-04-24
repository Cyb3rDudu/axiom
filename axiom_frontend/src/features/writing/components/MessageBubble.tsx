import React, { useState } from 'react'
import { MathMarkdown } from '../../../components/markdown/MathMarkdown'
import { Copy, RotateCcw, Check, Bot, User, Trash2, FileDown, FilePlus, FileCheck, BookMarked } from 'lucide-react'
import { formatChatMessageTime } from '../../../utils/timezone'
import { SourceBubbles } from './SourceBubbles'
import type { Source } from '../api'

interface MessageBubbleProps {
  message: {
    id: string
    role: 'user' | 'assistant'
    content: string
    timestamp: Date | string
    sources?: Source[]
  }
  onRegenerate?: () => void
  onDelete?: () => void
  isRegenerating?: boolean
  /** Replace the current draft content with the block text.
   *  Used by content-block:document blocks ("übernehmen"). */
  onApplyToDraft?: (content: string) => void | Promise<void>
  /** Append the block to the end of the current draft.
   *  Used by content-block:section / paragraph / list blocks. */
  onAppendToDraft?: (content: string) => void | Promise<void>
  /** One-click apply: replace draft with the document block and
   *  append all remaining section/paragraph/list blocks. Useful for
   *  Hausarbeit-style revision responses that come as [document +
   *  Literaturverzeichnis]. */
  onApplyAllBlocks?: (reconstructed: string) => void | Promise<void>
}

export const MessageBubble: React.FC<MessageBubbleProps> = ({
  message,
  onRegenerate,
  onDelete,
  isRegenerating = false,
  onApplyToDraft,
  onAppendToDraft,
  onApplyAllBlocks,
}) => {
  const [copiedContent, setCopiedContent] = useState<string | null>(null)
  const [hoveredCodeBlock, setHoveredCodeBlock] = useState<string | null>(null)

  // Precompute whether this message has a content-block:document that
  // the "Apply all blocks" button should act on. Only assistant
  // messages with at least one document-level block qualify — for
  // chat-only responses the granular per-block buttons remain the
  // only apply action.
  const hasDocumentBlock = React.useMemo(() => {
    if (message.role !== 'assistant') return false
    const regex = /```content-block:document\s*\n([\s\S]*?)\n```/g
    return regex.test(message.content || '')
  }, [message.content, message.role])

  // Build the reconstructed draft content: document block replaces,
  // subsequent section/paragraph/list blocks append. Skips code
  // blocks and freeform text between blocks — those are chat
  // commentary, not part of the deliverable.
  const reconstructAllBlocks = React.useCallback((): string | null => {
    if (!message.content) return null
    const regex = /```content-block:(\w+)\s*\n([\s\S]*?)\n```/g
    let documentBody: string | null = null
    const appendables: string[] = []
    let m: RegExpExecArray | null
    while ((m = regex.exec(message.content)) !== null) {
      const blockType = m[1]
      const body = (m[2] || '').trim()
      if (!body) continue
      if (blockType === 'code') continue
      // #78 — references is a data payload for the backend, not
      // draft content. Never include it in Apply-all-Blocks.
      if (blockType === 'references') continue
      if (blockType === 'document' && documentBody === null) {
        documentBody = body
      } else if (blockType !== 'document') {
        appendables.push(body)
      }
    }
    if (documentBody === null) return null
    if (appendables.length === 0) return documentBody
    return [documentBody, ...appendables].join('\n\n')
  }, [message.content])

  // Function to process content and highlight citations
  const processCitations = (text: string) => {
    // Pattern to match citations - handles both single [1] and multiple [1,2,3] or [1, 2, 3]
    const citationPattern = /\[(\d+(?:\s*,\s*\d+)*)\]/g
    
    // Split text by citations and create elements
    const parts: React.ReactNode[] = []
    let lastIndex = 0
    let match
    
    while ((match = citationPattern.exec(text)) !== null) {
      // Add text before citation
      if (match.index > lastIndex) {
        parts.push(text.slice(lastIndex, match.index))
      }
      
      // Parse the citation numbers (could be single or multiple)
      const citationText = match[1]
      const citationNumbers = citationText.split(',').map(num => num.trim())
      
      // Create appropriate title text
      const titleText = citationNumbers.length === 1 
        ? `Reference ${citationNumbers[0]}`
        : `References ${citationNumbers.join(', ')}`
      
      // Add citation with special styling
      parts.push(
        <span
          key={`citation-${match.index}`}
          className="inline-flex items-center px-1 py-0.5 mx-0.5 rounded text-xs font-semibold bg-primary/10 text-primary hover:bg-primary/20 transition-colors cursor-help"
          title={titleText}
        >
          [{citationText}]
        </span>
      )
      
      lastIndex = match.index + match[0].length
    }
    
    // Add remaining text
    if (lastIndex < text.length) {
      parts.push(text.slice(lastIndex))
    }
    
    return parts.length > 0 ? parts : text
  }

  const copyToClipboard = async (text: string, type: 'message' | 'code' = 'message') => {
    try {
      // Check if clipboard API is available
      if (navigator.clipboard && navigator.clipboard.writeText) {
        await navigator.clipboard.writeText(text)
      } else {
        // Fallback for older browsers or insecure contexts
        const textArea = document.createElement('textarea')
        textArea.value = text
        textArea.style.position = 'fixed'
        textArea.style.left = '-999999px'
        textArea.style.top = '-999999px'
        document.body.appendChild(textArea)
        textArea.focus()
        textArea.select()
        document.execCommand('copy')
        document.body.removeChild(textArea)
      }
      
      setCopiedContent(type === 'message' ? 'message' : text)
      setTimeout(() => setCopiedContent(null), 2000)
    } catch (error) {
      console.error('Failed to copy to clipboard:', error)
      // Still show success feedback even if copy failed
      setCopiedContent(type === 'message' ? 'message' : text)
      setTimeout(() => setCopiedContent(null), 2000)
    }
  }

  const isUserMessage = message.role === 'user'

  // Parse content into segments with inline content blocks
  const contentSegments = React.useMemo(() => {
    let content = message.content
    
    // Check if the entire content is wrapped in a markdown code block first
    const markdownCodeBlockRegex = /^```(?:markdown|md)?\s*\n([\s\S]*?)\n```$/
    const markdownMatch = content.match(markdownCodeBlockRegex)
    
    if (markdownMatch) {
      content = markdownMatch[1].trim()
    }
    
    // Split content by content blocks while preserving their positions
    const contentBlockRegex = /```content-block:(\w+)\s*\n([\s\S]*?)\n```/g
    const segments: Array<{type: 'text' | 'content-block', content: string, blockType?: string, id?: string}> = []
    
    let lastIndex = 0
    let match
    
    while ((match = contentBlockRegex.exec(content)) !== null) {
      // Add text before this content block
      if (match.index > lastIndex) {
        const textBefore = content.slice(lastIndex, match.index).trim()
        if (textBefore) {
          segments.push({
            type: 'text',
            content: textBefore
          })
        }
      }
      
      // Add the content block
      segments.push({
        type: 'content-block',
        content: match[2].trim(),
        blockType: match[1],
        id: `block-${Math.random().toString(36).substr(2, 9)}`
      })
      
      lastIndex = match.index + match[0].length
    }
    
    // Add any remaining text after the last content block
    if (lastIndex < content.length) {
      const textAfter = content.slice(lastIndex).trim()
      if (textAfter) {
        segments.push({
          type: 'text',
          content: textAfter
        })
      }
    }
    
    // If no content blocks found, treat entire content as text
    if (segments.length === 0) {
      segments.push({
        type: 'text',
        content: content
      })
    }
    
    return segments
  }, [message.content])

  return (
    <div className={`flex ${isUserMessage ? 'justify-end' : 'justify-start'}`}>
      <div className={`flex ${
        isUserMessage 
          ? 'max-w-xs lg:max-w-md flex-row-reverse' 
          : 'max-w-full flex-row'
      } items-start space-x-3`}>
        {/* Avatar */}
        <div className={`flex-shrink-0 w-7 h-7 rounded-full flex items-center justify-center ${
          isUserMessage 
            ? 'bg-primary text-primary-foreground ml-2' 
            : 'bg-muted mr-2'
        }`}>
          {isUserMessage ? (
            <User className="h-3.5 w-3.5" />
          ) : (
            <Bot className="h-3.5 w-3.5 text-text-secondary" />
          )}
        </div>
        
        {/* Message Container */}
        <div className={`relative group min-w-0 ${
          isUserMessage
            ? 'max-w-xs lg:max-w-md'
            : 'flex-1'
        }`}>
          <div className="absolute top-1.5 right-1.5 flex items-center space-x-1 opacity-0 group-hover:opacity-100 transition-opacity duration-200">
            {/* #46 — single-click apply for responses that carry a
                document block + any number of section/paragraph/list
                blocks. Shortcuts the otherwise 2-click FileDown +
                FilePlus dance for the common Hausarbeit shape. */}
            {hasDocumentBlock && onApplyAllBlocks && (
              <button
                onClick={async () => {
                  const reconstructed = reconstructAllBlocks()
                  if (reconstructed) {
                    await onApplyAllBlocks(reconstructed)
                  }
                }}
                className="bg-background border border-border rounded-md p-1 shadow-sm hover:bg-primary/10 hover:border-primary z-10 transition-colors duration-150"
                title="Ganze Antwort in Entwurf übernehmen (Haupttext ersetzen, weitere Blöcke anhängen)"
              >
                <FileCheck className="h-2.5 w-2.5 text-primary" />
              </button>
            )}
            <button
              onClick={() => copyToClipboard(message.content)}
              className="bg-background border border-border rounded-md p-1 shadow-sm hover:bg-muted z-10 transition-colors duration-150"
              title="Copy message"
            >
              {copiedContent === 'message' ? (
                <Check className="h-2.5 w-2.5 text-green-500" />
              ) : (
                <Copy className="h-2.5 w-2.5 text-text-secondary" />
              )}
            </button>
            {onDelete && (
              <button
                onClick={onDelete}
                className="bg-background border border-border rounded-md p-1 shadow-sm hover:bg-destructive/10 hover:border-destructive/20 z-10 transition-colors duration-150"
                title="Delete message"
              >
                <Trash2 className="h-2.5 w-2.5 text-destructive" />
              </button>
            )}
          </div>

          {/* Message Bubble */}
          <div className={`px-3 py-2 rounded-xl ${
            isUserMessage
              ? 'bg-primary text-primary-foreground rounded-br-md'
              : 'bg-card border border-border text-text-primary rounded-bl-md shadow-sm'
          }`}>
            <div className="prose prose-xs max-w-none text-current break-words text-sm">
              {/* Render content segments in their original order */}
              {contentSegments.map((segment, index) => {
                if (segment.type === 'text') {
                  return (
                    <div key={index}>
                      <MathMarkdown
                        content={segment.content}
                        className="prose prose-xs max-w-none"
                        components={{
                          // Links
                          a: ({node, ...props}) => (
                            <a 
                              {...props} 
                              target="_blank" 
                              rel="noopener noreferrer" 
                              className={isUserMessage ? "text-primary-foreground/80 hover:underline" : "text-primary hover:underline"}
                            />
                          ),
                          
                          // Lists
                          ul: ({node, ...props}) => <ul {...props} className="my-1 space-y-0.5" />,
                          ol: ({node, ...props}) => <ol {...props} className="my-1 space-y-0.5" />,
                          li: ({node, ...props}) => <li {...props} className="ml-4" />,
                          
                          // Paragraphs
                          p: ({node, children, ...props}) => {
                            // Process children to highlight citations
                            const processedChildren = React.Children.map(children, (child) => {
                              if (typeof child === 'string') {
                                return processCitations(child)
                              }
                              return child
                            })
                            
                            return (
                              <p {...props} className="mb-1 last:mb-0 break-words leading-relaxed">
                                {processedChildren}
                              </p>
                            )
                          },
                          
                          // Headings with consistent primary color
                          h1: ({node, ...props}) => <h1 {...props} className="text-lg font-bold mb-1 mt-2 first:mt-0 text-primary" />,
                          h2: ({node, ...props}) => <h2 {...props} className="text-base font-semibold mb-1 mt-1.5 first:mt-0 text-primary" />,
                          h3: ({node, ...props}) => <h3 {...props} className="text-sm font-medium mb-0.5 mt-1 first:mt-0 text-primary" />,
                          h4: ({node, ...props}) => <h4 {...props} className="text-xs font-medium mb-0.5 mt-0.5 first:mt-0 text-primary" />,
                          h5: ({node, ...props}) => <h5 {...props} className="text-xs font-medium mb-0.5 mt-0.5 first:mt-0 text-primary" />,
                          h6: ({node, ...props}) => <h6 {...props} className="text-xs font-medium mb-0.5 mt-0.5 first:mt-0 text-primary" />,
                          
                          // Blockquotes
                          blockquote: ({node, ...props}) => (
                            <blockquote 
                              {...props} 
                              className={`border-l-4 pl-4 my-1.5 italic ${
                                isUserMessage ? 'border-primary-foreground/50' : 'border-border'
                              }`} 
                            />
                          ),
                          
                          // Tables
                          table: ({node, ...props}) => (
                            <div className="overflow-x-auto my-3">
                              <table {...props} className="min-w-full border-collapse border border-border" />
                            </div>
                          ),
                          th: ({node, ...props}) => (
                            <th {...props} className="border border-border px-3 py-2 bg-muted/50 font-medium text-left text-foreground" />
                          ),
                          td: ({node, ...props}) => (
                            <td {...props} className="border border-border px-3 py-2" />
                          ),
                          
                          // Inline code
                          code: ({node, className, children, ...props}) => {
                            const match = /language-(\w+)/.exec(className || '')
                            const isInline = !match
                            const codeContent = String(children).replace(/\n$/, '')
                            
                            if (isInline) {
                              // Remove backticks from inline code
                              const processedChildren = React.Children.map(children, child => {
                                if (typeof child === 'string') {
                                  return child.replace(/`/g, '')
                                }
                                return child
                              })
                              
                              return (
                                <code 
                                  {...props} 
                                  className={`px-1 py-0.5 rounded text-xs font-mono ${
                                    isUserMessage 
                                      ? 'bg-primary-foreground/20 text-primary-foreground' 
                                      : 'bg-code-background text-code-foreground'
                                  }`}
                                >
                                  {processedChildren}
                                </code>
                              )
                            }
                            
                            // Code block with copy button
                            const blockId = `code-${Math.random().toString(36).substr(2, 9)}`
                            
                            return (
                              <div 
                                className="relative group/code my-3"
                                onMouseEnter={() => setHoveredCodeBlock(blockId)}
                                onMouseLeave={() => setHoveredCodeBlock(null)}
                              >
                                {/* Copy button for code block */}
                                <button
                                  onClick={() => copyToClipboard(codeContent, 'code')}
                                  className={`absolute top-1.5 right-1.5 transition-opacity duration-200 bg-gray-700 hover:bg-gray-600 text-white rounded p-1 text-xs ${
                                    hoveredCodeBlock === blockId ? 'opacity-100' : 'opacity-0'
                                  }`}
                                  title="Copy code"
                                >
                                  {copiedContent === codeContent ? (
                                    <Check className="h-2.5 w-2.5" />
                                  ) : (
                                    <Copy className="h-2.5 w-2.5" />
                                  )}
                                </button>
                                
                                <pre className="bg-code-background text-code-foreground p-3 rounded-lg overflow-x-auto">
                                  <code className="text-xs font-mono whitespace-pre">
                                    {children}
                                  </code>
                                </pre>
                                
                                {match && (
                                  <div className="text-xs text-text-tertiary mt-1 font-mono">
                                    {match[1]}
                                  </div>
                                )}
                              </div>
                            )
                          },
                          
                          // Pre blocks (fallback)
                          pre: ({node, children, ...props}) => {
                            // If it's already handled by code component, don't double-wrap
                            if (React.isValidElement(children) && children.type === 'code') {
                              return <>{children}</>
                            }
                            
                            return (
                              <pre 
                                {...props} 
                                className="bg-code-background text-code-foreground p-3 rounded-lg overflow-x-auto my-2 text-xs font-mono whitespace-pre-wrap"
                              >
                                {children}
                              </pre>
                            )
                          },
                          
                          // Horizontal rules
                          hr: ({node, ...props}) => (
                            <hr {...props} className="my-1.5 border-border" />
                          ),
                        }}
                      />
                    </div>
                  )
                } else if (segment.blockType === 'references') {
                  // #78 — the references block is a structured data payload
                  // the backend parses into draft_references via
                  // replace_draft_registry. Showing the raw JSON array in
                  // the chat is visual noise; collapse to a summary pill
                  // with a disclosure triangle for the rare case where
                  // the user wants to eyeball what went in.
                  let entryCount = 0
                  try {
                    const parsed = JSON.parse(segment.content)
                    if (Array.isArray(parsed)) entryCount = parsed.length
                  } catch {
                    // Malformed JSON — still show the pill so user sees
                    // something landed, but with a warning tone.
                  }
                  return (
                    <details
                      key={segment.id || index}
                      className="my-3 rounded border border-border bg-muted/30"
                    >
                      <summary className="cursor-pointer list-none px-3 py-2 text-xs flex items-center gap-2 hover:bg-muted/60 rounded">
                        <BookMarked className="h-3.5 w-3.5 text-primary" />
                        <span className="font-medium">Bibliography updated</span>
                        <span className="text-text-secondary">
                          · {entryCount} {entryCount === 1 ? 'entry' : 'entries'} sent to registry
                        </span>
                        <span className="text-text-tertiary ml-auto text-xs">show JSON</span>
                      </summary>
                      <div className="px-3 pb-3">
                        <pre className="bg-code-background text-code-foreground p-2 rounded overflow-x-auto text-xs font-mono whitespace-pre-wrap leading-relaxed">
                          {segment.content}
                        </pre>
                      </div>
                    </details>
                  )
                } else {
                  // Render content block inline
                  const isCode = segment.blockType === 'code'
                  // "Replace draft" (FileDown) is appropriate when the
                  // block is a full document revision. Append (FilePlus)
                  // is the right action for smaller granularity — a
                  // single section or paragraph the user wants to graft
                  // onto what's already in the editor.
                  const canReplace = !isCode &&
                    (segment.blockType === 'document') &&
                    typeof onApplyToDraft === 'function'
                  const canAppend = !isCode &&
                    segment.blockType !== 'document' &&
                    typeof onAppendToDraft === 'function'
                  return (
                    <div key={segment.id || index} className="relative my-3 group/content-block">
                      {/* Action row: Copy always, Replace/Append when the
                          block is applicable to the draft editor. */}
                      <div className="absolute top-1.5 right-1.5 z-10 flex items-center gap-1">
                        {canReplace && (
                          <button
                            onClick={() => onApplyToDraft!(segment.content)}
                            className="bg-background border border-border rounded-md p-1 shadow-sm hover:bg-primary/10 hover:border-primary text-primary"
                            title="Entwurf durch diesen Block ersetzen"
                          >
                            <FileDown className="h-2.5 w-2.5" />
                          </button>
                        )}
                        {canAppend && (
                          <button
                            onClick={() => onAppendToDraft!(segment.content)}
                            className="bg-background border border-border rounded-md p-1 shadow-sm hover:bg-primary/10 hover:border-primary text-primary"
                            title="An den Entwurf anhängen"
                          >
                            <FilePlus className="h-2.5 w-2.5" />
                          </button>
                        )}
                        <button
                          onClick={() => copyToClipboard(segment.content, 'code')}
                          className="bg-background border border-border rounded-md p-1 shadow-sm hover:bg-muted opacity-100"
                          title={`Copy ${segment.blockType} content`}
                        >
                          {copiedContent === segment.content ? (
                            <Check className="h-2.5 w-2.5 text-green-500" />
                          ) : (
                            <Copy className="h-2.5 w-2.5 text-text-secondary" />
                          )}
                        </button>
                      </div>

                      {/* Content block container */}
                      <div className="border border-border rounded-lg p-3 bg-muted">
                        <div className="text-xs text-text-tertiary mb-1.5 font-mono uppercase">
                          {segment.blockType} content
                        </div>
                        {/* Render code content blocks as raw code, others as markdown */}
                        {segment.blockType === 'code' ? (
                          <pre className="bg-code-background text-code-foreground p-3 rounded-lg overflow-x-auto text-xs font-mono whitespace-pre-wrap leading-relaxed">
                            {segment.content}
                          </pre>
                        ) : (
                          <div className="prose prose-xs max-w-none">
                            <MathMarkdown
                              content={segment.content}
                              className="prose prose-xs max-w-none"
                              components={{
                                // Same components as above but simplified for content blocks
                                a: ({node, ...props}) => (
                                  <a {...props} target="_blank" rel="noopener noreferrer" className="text-primary hover:underline" />
                                ),
                                ul: ({node, ...props}) => <ul {...props} className="my-2 space-y-1" />,
                                ol: ({node, ...props}) => <ol {...props} className="my-2 space-y-1" />,
                                li: ({node, ...props}) => <li {...props} className="ml-4" />,
                                p: ({node, children, ...props}) => {
                                  // Process children to highlight citations
                                  const processedChildren = React.Children.map(children, (child) => {
                                    if (typeof child === 'string') {
                                      return processCitations(child)
                                    }
                                    return child
                                  })
                                  
                                  return (
                                    <p {...props} className="mb-2 last:mb-0 break-words leading-relaxed">
                                      {processedChildren}
                                    </p>
                                  )
                                },
                                h1: ({node, ...props}) => <h1 {...props} className="text-lg font-bold mb-2 mt-3 first:mt-0 text-primary" />,
                                h2: ({node, ...props}) => <h2 {...props} className="text-base font-semibold mb-2 mt-2 first:mt-0 text-primary" />,
                                h3: ({node, ...props}) => <h3 {...props} className="text-sm font-medium mb-1 mt-2 first:mt-0 text-primary" />,
                                h4: ({node, ...props}) => <h4 {...props} className="text-xs font-medium mb-1 mt-1 first:mt-0 text-primary" />,
                                h5: ({node, ...props}) => <h5 {...props} className="text-xs font-medium mb-1 mt-1 first:mt-0 text-primary" />,
                                h6: ({node, ...props}) => <h6 {...props} className="text-xs font-medium mb-1 mt-1 first:mt-0 text-primary" />,
                                blockquote: ({node, ...props}) => (
                                  <blockquote {...props} className="border-l-4 border-border pl-4 my-3 italic" />
                                ),
                                code: ({node, className, children, ...props}) => {
                                  const match = /language-(\w+)/.exec(className || '')
                                  const isInline = !match
                                  
                                  if (isInline) {
                                    return (
                                      <code {...props} className="px-1 py-0.5 rounded text-xs font-mono bg-code-background text-code-foreground">
                                        {children}
                                      </code>
                                    )
                                  }
                                  
                                  return (
                                    <pre className="bg-code-background text-code-foreground p-3 rounded-lg overflow-x-auto my-2">
                                      <code className="text-xs font-mono whitespace-pre">
                                        {children}
                                      </code>
                                    </pre>
                                  )
                                },
                              }}
                            />
                          </div>
                        )}
                      </div>
                    </div>
                  )
                }
              })}
            </div>
            
            {/* Sources for assistant messages */}
            {!isUserMessage && message.sources && message.sources.length > 0 && (
              <SourceBubbles sources={message.sources} />
            )}
            
            {/* Timestamp */}
            <p className={`text-xs mt-1 ${
              isUserMessage ? 'text-primary-foreground/70' : 'text-text-tertiary'
            }`} style={{ fontSize: '0.7rem', opacity: 0.8 }}>
              {formatChatMessageTime(message.timestamp)}
            </p>
          </div>

          {/* Action buttons for assistant messages - Always visible */}
          {!isUserMessage && (
            <div className="flex items-center justify-end space-x-1.5 mt-1.5">
              {onRegenerate && (
                <button
                  onClick={onRegenerate}
                  disabled={isRegenerating}
                  className="flex items-center space-x-1 px-1.5 py-0.5 text-xs text-text-secondary hover:text-text-primary hover:bg-muted rounded transition-colors duration-200 disabled:opacity-50 disabled:cursor-not-allowed"
                  title="Regenerate response"
                >
                  <RotateCcw className={`h-2.5 w-2.5 ${isRegenerating ? 'animate-spin' : ''}`} />
                  <span>Regenerate</span>
                </button>
              )}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
