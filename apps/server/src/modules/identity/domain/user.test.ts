import { describe, expect, it } from 'vite-plus/test';
import {
  accessFor,
  canRevokeToken,
  disableDenial,
  type GroupPolicy,
  IdentityError,
  isActiveAdmin,
  isAdmin,
  type Principal,
  principalOf,
  provisionedGroups,
  type User,
} from './user.ts';

const policy: GroupPolicy = { loginGroup: 'mcp-users', adminGroup: 'mcp-admins' };

const user: User = {
  id: 'u1',
  oidcSub: 'sub-1',
  name: 'Anna',
  email: null,
  groups: ['mcp-users'],
  role: 'member',
  disabled: false,
  createdAt: new Date(0),
  lastLoginAt: null,
};

describe('accessFor', () => {
  it('lets members of the login group in as members', () => {
    expect(accessFor(['mcp-users'], policy)).toBe('member');
  });
  it('makes members of the admin group admins, even without the login group', () => {
    expect(accessFor(['mcp-admins'], policy)).toBe('admin');
    expect(accessFor(['mcp-users', 'mcp-admins'], policy)).toBe('admin');
  });
  it('refuses everyone else', () => {
    expect(accessFor(['family'], policy)).toBeNull();
    expect(accessFor([], policy)).toBeNull();
  });
});

describe('principalOf', () => {
  it('carries role and groups of the stored user', () => {
    expect(principalOf(user, 'session')).toEqual({
      userId: 'u1',
      name: 'Anna',
      role: 'member',
      groups: ['mcp-users'],
      via: 'session',
    });
  });
  it('adds tokenId for token principals', () => {
    expect(principalOf(user, 'token', 't1')).toMatchObject({ via: 'token', tokenId: 't1' });
  });
});

describe('IdentityError', () => {
  it('carries its code and retry hint', () => {
    expect(new IdentityError('rate_limited', 'm', 5)).toMatchObject({
      code: 'rate_limited',
      retryAfterSec: 5,
    });
  });
});

const who = (userId: string, role: Principal['role']): Principal => ({
  userId,
  name: userId,
  role,
  groups: [],
  via: 'session',
});

describe('authorization policy', () => {
  it('isAdmin follows the role', () => {
    expect(isAdmin(who('a', 'admin'))).toBe(true);
    expect(isAdmin(who('a', 'member'))).toBe(false);
  });
  it('tokens are revocable by their owner and by admins only', () => {
    expect(canRevokeToken(who('a', 'member'), 'a')).toBe(true);
    expect(canRevokeToken(who('b', 'member'), 'a')).toBe(false);
    expect(canRevokeToken(who('root', 'admin'), 'a')).toBe(true);
  });
  it('only admins set the disabled flag, and not on themselves', () => {
    expect(disableDenial(who('a', 'member'), 'b', true)?.code).toBe('forbidden');
    expect(disableDenial(who('root', 'admin'), 'root', true)?.code).toBe('forbidden');
    expect(disableDenial(who('root', 'admin'), 'root', false)).toBeNull();
    expect(disableDenial(who('root', 'admin'), 'b', true)).toBeNull();
  });
});

describe('isActiveAdmin', () => {
  it('an enabled admin only', () => {
    expect(isActiveAdmin({ ...user, role: 'admin' })).toBe(true);
    expect(isActiveAdmin(user)).toBe(false);
    expect(isActiveAdmin({ ...user, role: 'admin', disabled: true })).toBe(false);
    expect(isActiveAdmin(null)).toBe(false);
  });
});

describe('provisionedGroups', () => {
  it('a new person gets the login group, or the admin group with admin', () => {
    expect(provisionedGroups(null, { admin: false }, policy)).toEqual(['mcp-users']);
    expect(provisionedGroups(null, { admin: true }, policy)).toEqual(['mcp-admins']);
  });

  it('keeps the other groups of an existing person and swaps only the policy groups', () => {
    expect(provisionedGroups(['devs', 'mcp-admins'], { admin: false }, policy)).toEqual([
      'devs',
      'mcp-users',
    ]);
    expect(provisionedGroups(['devs', 'mcp-users'], { admin: true }, policy)).toEqual([
      'devs',
      'mcp-admins',
    ]);
  });

  it('gives a role through accessFor', () => {
    expect(accessFor(provisionedGroups(null, { admin: true }, policy), policy)).toBe('admin');
    expect(accessFor(provisionedGroups(null, { admin: false }, policy), policy)).toBe('member');
  });
});
