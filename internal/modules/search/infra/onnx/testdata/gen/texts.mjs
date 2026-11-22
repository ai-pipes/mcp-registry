// The text set of the reference data: the curated texts (the compat suite's passages and queries,
// adversarial strings, targeted Unicode cases) and a seeded fuzz set. Copied from the embedder
// spike's make-texts.mjs and fuzz-texts.mjs; nothing here depends on a CPU.
import { readFileSync } from 'node:fs';

export function curated(fixturesDir) {
  const texts = [];
  for (const f of ['gitea', 'youtrack']) {
    const j = JSON.parse(readFileSync(`${fixturesDir}/${f}.json`, 'utf8'));
    for (const t of j.tools ?? j)
      texts.push(`passage: ${f}. ${f} server\n${t.name} ${t.title ?? ''}\n${t.description ?? ''}`);
  }
  const queries = [
    'create an issue',
    'list pull requests in a repository',
    'найти задачу по номеру',
    'создать задачу в трекере',
    'what is the weather in Paris',
    'book a flight to Tokyo',
    'コードレビューを依頼する',
    '搜索仓库中的文件',
    'merge a branch 🚀🔥',
    'a  b   c    multiple     spaces',
    '  leading and trailing  ',
    'Ünïcödé façade naïve Ærøskøbing',
    'tabs\tand\nnewlines\r\nhere',
    'ＦＵＬＬＷＩＤＴＨ ｔｅｘｔ ①②③',
    'é combining vs é precomposed',
    'x',
    '',
    'user@example.com https://example.org/a?b=c&d=e#f',
    'SELECT * FROM t WHERE a <> 1; -- comment',
    'مرحبا بالعالم',
    'שלום עולם',
    'नमस्ते दुनिया',
    '안녕하세요 세계',
    'zero\u200bwidth nbsp\u00a0\ufeffbom',
  ];

  for (const q of queries) texts.push(`query: ${q}`);
  texts.push(
    'passage: ' + 'The quick brown fox jumps over the lazy dog. Быстрая лиса. '.repeat(120),
  );
  texts.push('query: ' + 'длинный запрос про репозиторий и задачи '.repeat(200));
  const adv = [
    '',
    ' ',
    '   ',
    '\t',
    '\n',
    '\r\n',
    '\u3000',
    '\u00a0',
    '\u200b',
    '\ufeff',
    '\u0000',
    'a\u0000b',
    '\u0001\u0002\u0003',
    'bell\u0007tab\u000bff\u000c',
    '\u001b[31mred\u001b[0m',
    'del\u007fete',
    '\u0085next line',
    '\u2028line sep\u2029para',
    'soft\u00adhyphen',
    'Привет, мир!',
    'Ёжик в тумане ёлка Ё',
    'СОЗДАТЬ ЗАДАЧУ',
    'украї́нська мова: ї, є, ґ',
    'Ελληνικά κείμενα',
    'Հայերեն',
    'ქართული',
    '日本語のテキストです。',
    '中文文本，测试。',
    '繁體中文測試',
    '한국어 텍스트 테스트',
    'ﾊﾝｶｸｶﾀｶﾅ',
    '㍿ ㌔ ㎏ ㏂',
    '🚀',
    '👍🏽',
    '👨‍👩‍👧‍👦 family',
    '🏳️‍🌈 flag',
    '🇷🇸🇺🇸',
    '❤️‍🔥',
    '1️⃣ keycap',
    '😀😃😄😁😆😅🤣😂🙂🙃',
    'e\u0301',
    'n\u0303',
    'a\u0300\u0301\u0302\u0303\u0304',
    'Z̴̡̢̛͓̩a̷l̸g̵o̶',
    'ǅ ǈ ǋ titlecase',
    'ﬁ ﬂ ligatures',
    'Å Å Å',
    '½ ¼ ¾ ⅓',
    'x² H₂O',
    'Ⅻ ⅻ roman',
    '𝐁𝐨𝐥𝐝 𝕯𝖔𝖚𝖇𝖑𝖊',
    'İstanbul ıi',
    'straße STRASSE',
    'ǰ̌',
    '\ud800 lone surrogate',
    'end lone \udfff',
    'mixed: create задачу 作成 タスク 🔥',
    'repo/путь/文件.go:42',
    'C:\\Users\\name\\file.txt',
    '<script>alert(1)</script>',
    '{"json": [1, 2, 3]}',
    '<s>',
    '</s>',
    '<pad>',
    '<unk>',
    '<mask>',
    'text <mask> more',
    'a<s>b</s>c',
    ' <mask>',
    '▁underscore▁char',
    '▁',
    '__init__.py',
    'snake_case_name camelCaseName PascalCase',
    'v1.2.3-beta+build.4',
    '1234567890',
    '3.14159 2.71828 -0.5e-10',
    '#hashtag @mention $var %percent',
    '!!!???...',
    '…',
    '—–-',
    '“quotes” ‘single’ «guillemets»',
    'a'.repeat(600),
    'ab '.repeat(400),
    '日本'.repeat(700),
    '🚀'.repeat(600),
    'слово '.repeat(600),
    'word '.repeat(508).trim(),
    'word '.repeat(509).trim(),
    'word '.repeat(510).trim(),
    'word '.repeat(511).trim(),
    'x'.repeat(10000),
    ' '.repeat(1000),
    '\n'.repeat(100) + 'z',
  ];

  for (const a of adv) texts.push(a);
  for (const a of adv.slice(0, 60)) texts.push(`query: ${a}`);
  for (const a of adv.slice(60)) texts.push(`passage: ${a}`);
  return texts; // 296 texts
}

// Targeted strings: NFD Vietnamese, Arabic harakat, Hebrew niqqud, Thai, Indic, stacked marks, NUL.
export const extra = [
  '\ufffd',
  'a\ufffdb',
  'a\u0300',
  'a\u0300\u0301',
  'a\u0300\u0301\u0302',
  'o\u0308\u0301',
  'e\u0301\u0301',
  'x\u0300\u0301',
  '\u0301',
  'q\u0323\u0307',
  'a\u0000',
  '\u0000b',
  'ok',
  'Tiếng Việt có dấu',
  'Tiếng Việt có dấu'.normalize('NFD'),
  'Thành phố Hồ Chí Minh',
  'Thành phố Hồ Chí Minh'.normalize('NFD'),
  'مُحَمَّدٌ رَسُولُ',
  'בְּרֵאשִׁית בָּרָא',
  'กั้น ที่นี่ น้ำ',
  'हिन्दी में क्षमा',
  'ਪੰਜਾਬੀ',
  'தமிழ்நாடு',
  'ລາວ',
  'မြန်မာ',
  'བོད་ཡིག',
  'Ελληνικά ᾷ ἄ ῥ',
  'ǖ ǘ ǚ ǜ pinyin',
  'Đặng Thị Ngọc',
  'café',
  'naïve résumé',
  'Ångström',
  'ối giời ơi',
  'Ễ ễ Ệ ệ',
  'ṩ ḍ̇',
  'x́́',
];

// Seeded fuzz: random strings from many Unicode blocks, combining sequences, NFD of real words,
// whitespace and punctuation. No NUL and no lone surrogates. About 5750 unique strings.
export function fuzz() {
  let seed = 12345;
  const rnd = () => {
    seed = (seed + 0x6d2b79f5) >>> 0;
    let t = seed;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
  const pick = (a) => a[Math.floor(rnd() * a.length)];
  const ranges = [
    [0x20, 0x7e],
    [0xa0, 0x24f],
    [0x300, 0x36f],
    [0x370, 0x3ff],
    [0x400, 0x4ff],
    [0x590, 0x5ff],
    [0x600, 0x6ff],
    [0x900, 0x97f],
    [0xe00, 0xe7f],
    [0x1100, 0x11ff],
    [0x1e00, 0x1eff],
    [0x2000, 0x206f],
    [0x2100, 0x22ff],
    [0x3000, 0x30ff],
    [0x4e00, 0x4fff],
    [0xac00, 0xad00],
    [0xf900, 0xfaff],
    [0xfb00, 0xfb4f],
    [0xfe00, 0xfe6f],
    [0xff00, 0xffef],
    [0x1d400, 0x1d4ff],
    [0x1f300, 0x1f6ff],
    [0x1f900, 0x1f9ff],
    [0x1, 0x1f],
    [0xe000, 0xe0ff],
  ];
  const words = [
    'Tiếng Việt',
    'Hồ Chí Minh',
    'phở bò',
    'Nguyễn',
    'résumé',
    'Ångström',
    'São Paulo',
    'Zürich',
    'œuvre',
    'ﬁnancial',
    'Ǆemal',
    'İzmir',
    'ğüşıöç',
    'Ελλάδα',
    'Ὀδυσσεύς',
    'Москва',
    'Ёлка',
    'Київ',
    'עִבְרִית',
    'العَرَبِيَّة',
    'हिन्दी',
    'ไทย',
    '日本語',
    '한국어',
    '中文',
  ];
  const out = [];
  for (let n = 0; n < 6600; n++) {
    const kind = n % 4;
    let s = '';
    if (kind === 0) {
      const len = 1 + Math.floor(rnd() * 20);
      for (let k = 0; k < len; k++) {
        const [a, b] = pick(ranges);
        const cp = a + Math.floor(rnd() * (b - a + 1));
        if (cp === 0 || (cp >= 0xd800 && cp <= 0xdfff)) continue;
        s += String.fromCodePoint(cp);
        if (rnd() < 0.2) s += ' ';
      }
    } else if (kind === 1) {
      const len = 1 + Math.floor(rnd() * 6);
      for (let k = 0; k < len; k++) {
        s += pick('aeiouyAEOUnczsxqgdk');
        const m = Math.floor(rnd() * 4);
        for (let j = 0; j < m; j++) s += String.fromCodePoint(0x300 + Math.floor(rnd() * 0x70));
        if (rnd() < 0.3) s += ' ';
      }
    } else if (kind === 2) {
      const w = pick(words);
      s =
        (rnd() < 0.5 ? w.normalize('NFD') : rnd() < 0.5 ? w.normalize('NFKD') : w) +
        ' ' +
        pick(words).normalize(rnd() < 0.5 ? 'NFC' : 'NFD');
    } else {
      const len = 1 + Math.floor(rnd() * 12);
      for (let k = 0; k < len; k++)
        s += pick([
          ' ',
          '  ',
          '\t',
          '\n',
          '　',
          ' ',
          '.',
          ',',
          '!',
          '?',
          '-',
          '_',
          '▁',
          '…',
          'word',
          'слово',
          '字',
          '🚀',
          '<mask>',
          '<s>',
          '</s>',
          '<unk>',
          '<pad>',
        ]);
    }
    if (!out.includes('query: ' + s) && !out.includes('passage: ' + s))
      out.push(rnd() < 0.5 ? 'query: ' + s : 'passage: ' + s);
  }
  return out;
}
