import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

export const c = [postgres, randomUUID];
