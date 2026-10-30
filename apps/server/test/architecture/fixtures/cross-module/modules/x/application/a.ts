// Fixture: reaching into another module's domain bypasses its index.ts.
import { b } from '../../y/domain/b.ts';

export const a = b;
