import React, { useState, useRef, useEffect, useLayoutEffect } from 'react'

interface DropdownMenuProps {
  children: React.ReactNode
}

interface DropdownMenuTriggerProps {
  asChild?: boolean
  children: React.ReactNode
}

interface DropdownMenuContentProps {
  align?: 'start' | 'center' | 'end'
  className?: string
  children: React.ReactNode
}

interface DropdownMenuItemProps {
  onClick?: (e: React.MouseEvent) => void
  className?: string
  children: React.ReactNode
}

// Context for sharing state between components
const DropdownContext = React.createContext<{
  isOpen: boolean
  setIsOpen: (open: boolean) => void
}>({
  isOpen: false,
  setIsOpen: () => {}
})

export const DropdownMenu: React.FC<DropdownMenuProps> = ({ children }) => {
  const [isOpen, setIsOpen] = useState(false)
  const containerRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const handleClickOutside = (event: MouseEvent) => {
      if (containerRef.current && !containerRef.current.contains(event.target as Node)) {
        setIsOpen(false)
      }
    }

    if (isOpen) {
      document.addEventListener('mousedown', handleClickOutside)
      return () => document.removeEventListener('mousedown', handleClickOutside)
    }
  }, [isOpen])

  return (
    <DropdownContext.Provider value={{ isOpen, setIsOpen }}>
      <div ref={containerRef} className="relative">
        {children}
      </div>
    </DropdownContext.Provider>
  )
}

export const DropdownMenuTrigger: React.FC<DropdownMenuTriggerProps> = ({ 
  asChild, 
  children
}) => {
  const { isOpen, setIsOpen } = React.useContext(DropdownContext)

  const handleClick = (e: React.MouseEvent) => {
    e.stopPropagation()
    // console.log('DropdownMenuTrigger clicked, current isOpen:', isOpen)
    setIsOpen(!isOpen)
  }

  if (asChild && React.isValidElement(children)) {
    return React.cloneElement(children, {
      onClick: handleClick
    } as any)
  }

  return (
    <button onClick={handleClick}>
      {children}
    </button>
  )
}

export const DropdownMenuContent: React.FC<DropdownMenuContentProps> = ({
  align = 'start',
  className = '',
  children
}) => {
  const { isOpen } = React.useContext(DropdownContext)
  const contentRef = useRef<HTMLDivElement>(null)
  // Vertical placement. "bottom" opens below the trigger (default),
  // "top" opens above. We measure on open and flip up when the content
  // won't fit below but has room above — prevents the dropdown from
  // disappearing behind fixed-bottom toolbars like the writing-chat
  // composer.
  const [placement, setPlacement] = useState<'bottom' | 'top'>('bottom')

  useLayoutEffect(() => {
    if (!isOpen || !contentRef.current) return
    const el = contentRef.current
    const parent = el.parentElement
    if (!parent) return
    const rect = parent.getBoundingClientRect()
    const spaceBelow = window.innerHeight - rect.bottom
    const spaceAbove = rect.top
    // Measure actual content height in the initial bottom-placement so
    // the comparison is honest. A 16 px cushion keeps the dropdown
    // slightly inside the viewport when it just barely fits.
    const neededHeight = el.offsetHeight + 16
    if (neededHeight > spaceBelow && spaceAbove >= spaceBelow) {
      setPlacement('top')
    } else {
      setPlacement('bottom')
    }
  }, [isOpen, children])

  if (!isOpen) return null

  const verticalClasses =
    placement === 'top' ? 'bottom-full mb-1' : 'top-full mt-1'

  return (
    <div
      ref={contentRef}
      className={`absolute ${verticalClasses} min-w-[12rem] overflow-hidden rounded-md border border-gray-200 bg-white p-1 text-gray-950 shadow-md z-50 whitespace-nowrap ${
        align === 'end' ? 'right-0' : align === 'center' ? 'left-1/2 transform -translate-x-1/2' : 'left-0'
      } ${className}`}
    >
      {children}
    </div>
  )
}

export const DropdownMenuItem: React.FC<DropdownMenuItemProps> = ({ 
  onClick,
  className = '',
  children
}) => {
  const { setIsOpen } = React.useContext(DropdownContext)

  const handleClick = (e: React.MouseEvent) => {
    // console.log('DropdownMenuItem clicked!', children)
    e.stopPropagation()
    
    // Execute the onClick handler first
    if (onClick) {
      // console.log('Executing onClick handler...')
      onClick(e)
    }
    
    // Then close the dropdown
    // console.log('Closing dropdown...')
    setIsOpen(false)
  }

  return (
    <div
      className={`relative flex cursor-pointer select-none items-center rounded-sm px-2 py-1.5 text-sm outline-none transition-colors hover:bg-gray-100 focus:bg-gray-100 ${className}`}
      onClick={handleClick}
    >
      {children}
    </div>
  )
}
