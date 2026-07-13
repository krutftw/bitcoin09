// Fixed Version 11-L QR encoder for short Nine Inbox pairing URLs.
// Byte mode capacity is 321 UTF-8 bytes. Four equal Reed-Solomon blocks keep
// this small enough to review while still covering every generated pair link.

const VERSION = 11;
const SIZE = 17 + VERSION * 4;
const DATA_CODEWORDS = 324;
const BLOCKS = 4;
const DATA_PER_BLOCK = 81;
const ECC_PER_BLOCK = 20;

function appendBits(bits, value, length) {
  for (let i = length - 1; i >= 0; i -= 1) bits.push(((value >>> i) & 1) !== 0);
}

function dataCodewords(text) {
  const bytes = new TextEncoder().encode(text);
  if (bytes.length > 321) throw new Error("Pairing link is too long for the QR code.");
  const bits = [];
  appendBits(bits, 0x4, 4);
  appendBits(bits, bytes.length, 16);
  for (const byte of bytes) appendBits(bits, byte, 8);
  const capacity = DATA_CODEWORDS * 8;
  appendBits(bits, 0, Math.min(4, capacity - bits.length));
  while (bits.length % 8 !== 0) bits.push(false);
  const result = [];
  for (let i = 0; i < bits.length; i += 8) {
    let value = 0;
    for (let j = 0; j < 8; j += 1) value = (value << 1) | Number(bits[i + j]);
    result.push(value);
  }
  for (let pad = 0; result.length < DATA_CODEWORDS; pad += 1) result.push(pad % 2 === 0 ? 0xec : 0x11);
  return result;
}

function multiply(left, right) {
  let x = left;
  let y = right;
  let result = 0;
  while (y !== 0) {
    if (y & 1) result ^= x;
    y >>>= 1;
    x <<= 1;
    if (x & 0x100) x ^= 0x11d;
  }
  return result;
}

function rsDivisor(degree) {
  const result = new Uint8Array(degree);
  result[degree - 1] = 1;
  let root = 1;
  for (let i = 0; i < degree; i += 1) {
    for (let j = 0; j < degree; j += 1) {
      result[j] = multiply(result[j], root);
      if (j + 1 < degree) result[j] ^= result[j + 1];
    }
    root = multiply(root, 2);
  }
  return result;
}

function rsRemainder(data, divisor) {
  const result = new Uint8Array(divisor.length);
  for (const byte of data) {
    const factor = byte ^ result[0];
    result.copyWithin(0, 1);
    result[result.length - 1] = 0;
    for (let i = 0; i < result.length; i += 1) result[i] ^= multiply(divisor[i], factor);
  }
  return result;
}

function interleavedCodewords(data) {
  const divisor = rsDivisor(ECC_PER_BLOCK);
  const blocks = [];
  for (let block = 0; block < BLOCKS; block += 1) {
    const bytes = Uint8Array.from(data.slice(block * DATA_PER_BLOCK, (block + 1) * DATA_PER_BLOCK));
    blocks.push({ data: bytes, ecc: rsRemainder(bytes, divisor) });
  }
  const result = [];
  for (let i = 0; i < DATA_PER_BLOCK; i += 1) for (const block of blocks) result.push(block.data[i]);
  for (let i = 0; i < ECC_PER_BLOCK; i += 1) for (const block of blocks) result.push(block.ecc[i]);
  return result;
}

function emptyMatrix() {
  return {
    modules: Array.from({ length: SIZE }, () => Array(SIZE).fill(false)),
    functions: Array.from({ length: SIZE }, () => Array(SIZE).fill(false)),
  };
}

function setFunction(matrix, x, y, dark) {
  if (x < 0 || y < 0 || x >= SIZE || y >= SIZE) return;
  matrix.modules[y][x] = dark;
  matrix.functions[y][x] = true;
}

function drawFinder(matrix, centerX, centerY) {
  for (let dy = -4; dy <= 4; dy += 1) {
    for (let dx = -4; dx <= 4; dx += 1) {
      const distance = Math.max(Math.abs(dx), Math.abs(dy));
      setFunction(matrix, centerX + dx, centerY + dy, distance !== 2 && distance !== 4);
    }
  }
}

function drawAlignment(matrix, centerX, centerY) {
  for (let dy = -2; dy <= 2; dy += 1) {
    for (let dx = -2; dx <= 2; dx += 1) setFunction(matrix, centerX + dx, centerY + dy, Math.max(Math.abs(dx), Math.abs(dy)) !== 1);
  }
}

function formatBits(mask) {
  const data = (1 << 3) | mask;
  let remainder = data;
  for (let i = 0; i < 10; i += 1) remainder = (remainder << 1) ^ ((remainder >>> 9) * 0x537);
  return ((data << 10) | remainder) ^ 0x5412;
}

function drawFormat(matrix, mask) {
  const bits = formatBits(mask);
  const bit = (index) => ((bits >>> index) & 1) !== 0;
  for (let i = 0; i <= 5; i += 1) setFunction(matrix, 8, i, bit(i));
  setFunction(matrix, 8, 7, bit(6));
  setFunction(matrix, 8, 8, bit(7));
  setFunction(matrix, 7, 8, bit(8));
  for (let i = 9; i < 15; i += 1) setFunction(matrix, 14 - i, 8, bit(i));
  for (let i = 0; i < 8; i += 1) setFunction(matrix, SIZE - 1 - i, 8, bit(i));
  for (let i = 8; i < 15; i += 1) setFunction(matrix, 8, SIZE - 15 + i, bit(i));
  setFunction(matrix, 8, SIZE - 8, true);
}

function drawVersion(matrix) {
  let remainder = VERSION;
  for (let i = 0; i < 12; i += 1) remainder = (remainder << 1) ^ ((remainder >>> 11) * 0x1f25);
  const bits = (VERSION << 12) | remainder;
  for (let i = 0; i < 18; i += 1) {
    const dark = ((bits >>> i) & 1) !== 0;
    const a = SIZE - 11 + (i % 3);
    const b = Math.floor(i / 3);
    setFunction(matrix, a, b, dark);
    setFunction(matrix, b, a, dark);
  }
}

function functionPatterns(mask) {
  const matrix = emptyMatrix();
  drawFinder(matrix, 3, 3);
  drawFinder(matrix, SIZE - 4, 3);
  drawFinder(matrix, 3, SIZE - 4);
  for (let i = 8; i < SIZE - 8; i += 1) {
    setFunction(matrix, 6, i, i % 2 === 0);
    setFunction(matrix, i, 6, i % 2 === 0);
  }
  const positions = [6, 30, 54];
  for (const y of positions) for (const x of positions) {
    const overlapsFinder = (x === 6 && y === 6) || (x === 6 && y === 54) || (x === 54 && y === 6);
    if (!overlapsFinder) drawAlignment(matrix, x, y);
  }
  drawFormat(matrix, mask);
  drawVersion(matrix);
  return matrix;
}

function maskBit(mask, x, y) {
  switch (mask) {
    case 0: return (x + y) % 2 === 0;
    case 1: return y % 2 === 0;
    case 2: return x % 3 === 0;
    case 3: return (x + y) % 3 === 0;
    case 4: return (Math.floor(y / 2) + Math.floor(x / 3)) % 2 === 0;
    case 5: return ((x * y) % 2 + (x * y) % 3) === 0;
    case 6: return ((x * y) % 2 + (x * y) % 3) % 2 === 0;
    case 7: return ((x + y) % 2 + (x * y) % 3) % 2 === 0;
    default: throw new Error("Invalid QR mask.");
  }
}

function drawData(matrix, codewords, mask) {
  const bits = [];
  for (const byte of codewords) appendBits(bits, byte, 8);
  let index = 0;
  let upward = true;
  for (let right = SIZE - 1; right >= 1; right -= 2) {
    if (right === 6) right -= 1;
    for (let vertical = 0; vertical < SIZE; vertical += 1) {
      const y = upward ? SIZE - 1 - vertical : vertical;
      for (let offset = 0; offset < 2; offset += 1) {
        const x = right - offset;
        if (matrix.functions[y][x]) continue;
        const value = index < bits.length && bits[index];
        matrix.modules[y][x] = Boolean(value) !== maskBit(mask, x, y);
        index += 1;
      }
    }
    upward = !upward;
  }
  if (index !== codewords.length * 8) throw new Error(`QR data placement failed (${index} of ${codewords.length * 8} bits).`);
}

function penalty(matrix) {
  const modules = matrix.modules;
  let score = 0;
  const lines = [...modules, ...Array.from({ length: SIZE }, (_, x) => modules.map((row) => row[x]))];
  for (const line of lines) {
    let run = 1;
    for (let i = 1; i <= SIZE; i += 1) {
      if (i < SIZE && line[i] === line[i - 1]) run += 1;
      else { if (run >= 5) score += 3 + run - 5; run = 1; }
    }
    for (let i = 0; i <= SIZE - 7; i += 1) {
      if (line.slice(i, i + 7).map(Number).join("") === "1011101") {
        const before = i >= 4 && line.slice(i - 4, i).every((value) => !value);
        const after = i + 11 <= SIZE && line.slice(i + 7, i + 11).every((value) => !value);
        if (before || after) score += 40;
      }
    }
  }
  for (let y = 0; y < SIZE - 1; y += 1) for (let x = 0; x < SIZE - 1; x += 1) {
    const value = modules[y][x];
    if (modules[y][x + 1] === value && modules[y + 1][x] === value && modules[y + 1][x + 1] === value) score += 3;
  }
  const dark = modules.flat().filter(Boolean).length;
  score += Math.floor(Math.abs(dark * 20 - SIZE * SIZE * 10) / (SIZE * SIZE)) * 10;
  return score;
}

export function encodeQR(text) {
  const codewords = interleavedCodewords(dataCodewords(text));
  let best;
  for (let mask = 0; mask < 8; mask += 1) {
    const matrix = functionPatterns(mask);
    drawData(matrix, codewords, mask);
    const score = penalty(matrix);
    if (!best || score < best.score) best = { score, modules: matrix.modules };
  }
  return best.modules;
}

export function renderPairingQR(container, text) {
  const matrix = encodeQR(text);
  const quiet = 4;
  const namespace = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(namespace, "svg");
  svg.setAttribute("viewBox", `0 0 ${SIZE + quiet * 2} ${SIZE + quiet * 2}`);
  svg.setAttribute("shape-rendering", "crispEdges");
  svg.setAttribute("aria-hidden", "true");
  const background = document.createElementNS(namespace, "rect");
  background.setAttribute("width", "100%");
  background.setAttribute("height", "100%");
  background.setAttribute("fill", "#fff");
  svg.append(background);
  let pathData = "";
  for (let y = 0; y < SIZE; y += 1) for (let x = 0; x < SIZE; x += 1) if (matrix[y][x]) pathData += `M${x + quiet},${y + quiet}h1v1h-1z`;
  const path = document.createElementNS(namespace, "path");
  path.setAttribute("d", pathData);
  path.setAttribute("fill", "#111715");
  svg.append(path);
  container.replaceChildren(svg);
}
