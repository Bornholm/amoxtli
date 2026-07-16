/** Options for the client. */
interface Options {
  retries: number;
}

type ID = string | number;

enum Color {
  Red,
  Green,
}

/** Parse an identifier. */
function parse(raw: string): ID {
  return raw;
}

/** A minimal HTTP client. */
class Client {
  /** Fetch the given URL. */
  fetch(url: string): Promise<Options> {
    return Promise.reject(new Error(url));
  }
}

export const defaultOptions: Options = { retries: 3 };
