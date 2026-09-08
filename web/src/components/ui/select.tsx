import * as React from 'react';
import { cn } from '../../lib/utils';

/**
 * A styled native select. Children are intentionally passed through unchanged
 * so option values, labels, disabled states, and optgroups remain browser-native.
 */
export const NativeSelect = React.forwardRef<HTMLSelectElement, React.ComponentProps<'select'>>(
  ({ className, children, ...props }, ref) => (
    <select
      ref={ref}
      className={cn(
        'flex h-10 w-full appearance-auto rounded-md border border-input bg-background px-3 py-2 text-sm ring-offset-background focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:cursor-not-allowed disabled:opacity-50',
        className,
      )}
      {...props}
    >
      {children}
    </select>
  ),
);

NativeSelect.displayName = 'NativeSelect';
