// SPDX-License-Identifier: AGPL-3.0-or-later
// 二维码（uqr，MIT）。以内联 SVG 路径绘制，不使用 data: URL 或内联样式，满足 CSP（DEP-05）。
// 二维码固定为白底黑码，深色主题下同样可以被扫描。
import { useMemo } from 'react';
import { encode } from 'uqr';

export interface QrCodeProps {
  value: string;
  /** 屏幕阅读器读出的说明 */
  label: string;
  size?: number;
}

export function QrCode({ value, label, size = 192 }: QrCodeProps) {
  const { d, n } = useMemo(() => {
    const qr = encode(value, { ecc: 'M', border: 2 });
    let path = '';
    qr.data.forEach((row, y) =>
      row.forEach((on, x) => {
        if (on) path += `M${x} ${y}h1v1h-1z`;
      }),
    );
    return { d: path, n: qr.size };
  }, [value]);
  return (
    <svg
      role="img"
      aria-label={label}
      width={size}
      height={size}
      viewBox={`0 0 ${n} ${n}`}
      shapeRendering="crispEdges"
      className="rounded-md bg-white"
    >
      <rect width={n} height={n} fill="#ffffff" />
      <path d={d} fill="#000000" />
    </svg>
  );
}
