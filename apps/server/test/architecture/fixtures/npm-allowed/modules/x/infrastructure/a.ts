import { randomUUID } from 'node:crypto';
import postgres from 'postgres';
import '@modelcontextprotocol/server';
import 'openid-client';
import 'ajv';

export const a = [postgres, randomUUID];
