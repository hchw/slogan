import '@testing-library/jest-dom/vitest';
import { cleanup } from '@testing-library/react';
import { afterEach } from 'vitest';

// Without vitest globals, testing-library does not register its automatic
// cleanup, so rendered trees would leak between tests and make queries
// ambiguous.
afterEach(() => {
  cleanup();
});
