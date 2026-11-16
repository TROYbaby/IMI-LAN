// 没有 Apple 开发者证书时的兜底：对产物做 ad-hoc 签名。
// Apple Silicon 上未签名的 .app 会被系统直接终止，ad-hoc 签名即可本地运行
// （首次打开仍需右键 -> 打开，绕过 Gatekeeper 提示）。
const { execFileSync } = require('child_process');
const path = require('path');
const fs = require('fs');

exports.default = async function (context) {
  if (process.platform !== 'darwin') return;
  // universal 构建会先把 x64 / arm64 各打一份到 *-x64-temp / *-arm64-temp，合并前不能签名：
  // _CodeSignature/CodeResources 两边不一样，@electron/universal 会因 SHA 不一致而失败。
  // 合并后的最终包还会再调一次 afterPack，那一次才签。
  if (/-(x64|arm64)-temp$/.test(context.appOutDir)) return;

  const appPath = path.join(context.appOutDir, `${context.packager.appInfo.productFilename}.app`);
  const enginePath = path.join(appPath, 'Contents', 'Resources', 'go2rtc');
  // electron-builder 复制文件一般保留权限，这里保险起见
  try { fs.chmodSync(enginePath, 0o755); } catch {}
  // go2rtc 是 lipo 合并出来的裸可执行文件，原来 Go 链接器加的 ad-hoc 签名已失效；
  // 它不在 Contents/Frameworks 等标准位置，--deep 不会签它，必须单独签。
  // 否则 Apple Silicon 上引擎会被系统 kill。
  execFileSync('codesign', ['--force', '--sign', '-', enginePath], { stdio: 'inherit' });
  execFileSync('codesign', ['--force', '--deep', '--sign', '-', appPath], { stdio: 'inherit' });
};
