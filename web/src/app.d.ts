// What a failed load can carry to the error page besides a sentence.
// https://svelte.dev/docs/kit/types#app.d.ts
declare global {
  namespace App {
    interface Error {
      message: string;
      /** The zone a page asked for and this server does not hold. */
      zone?: string;
      /** The zone it does hold that the address most likely meant. */
      nearest?: string;
    }
  }
}

export {};
