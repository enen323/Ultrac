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
  PACKAGE_URL: string;
  NATIVE_PACKAGE_URL: string;
  BUILD_TIME: string;
  FEEDBACK_CHANNEL: string;
  ISSUES_EXPLAINER: string;
};
