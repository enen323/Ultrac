declare namespace NodeJS {
  interface ProcessEnv {
    [key: string]: string | undefined;
  }
}

declare var process: {
  argv: string[];
  env: NodeJS.ProcessEnv;
  exit(code?: number): never;
};

declare const MACRO: {
  VERSION: string;
};
