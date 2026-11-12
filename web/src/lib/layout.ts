// The page column every signed-in screen shares, so the header, the server band and the
// page content keep one left edge from page to page.

/** Centres the column and gives it the side gutters. */
export const column = 'mx-auto w-full max-w-6xl px-4 sm:px-6';

/** A page's main landmark at the full column width, for lists and tables. */
export const pageWide = `${column} grid gap-6 py-6`;

/** The same column with its content held to a readable form width at the left edge. */
export const pageNarrow = `${pageWide} grid-cols-[minmax(0,48rem)]`;
