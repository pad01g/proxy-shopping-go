// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {ISafeModuleTarget} from "./interfaces/ISafeModuleTarget.sol";
import {IERC20} from "./interfaces/IERC20.sol";

/// 注文ごとの Safe（U, S, E の 2-of-3）に、タイムロックの抜け道を足すモジュール。
/// すべての注文の Safe で 1 つを共用する。
///   - t1 以降: shopper が単独で token の残高を全額受け取れる
///   - t2 以降: user が単独で token の残高を全額取り戻せる
contract PSEscrowModule {
    struct Config {
        address token;
        address user;
        address shopper;
        uint64 t1;
        uint64 t2;
    }

    mapping(address => Config) private configs;

    event Registered(address indexed safe, address indexed token, address user, address shopper, uint64 t1, uint64 t2);
    event ClaimedByShopper(address indexed safe, address indexed shopper, uint256 amount);
    event RefundedToUser(address indexed safe, address indexed user, uint256 amount);

    error AlreadyRegistered();
    error InvalidConfig();
    error NotRegistered();
    error Unauthorized();
    error TooEarly();
    error TransferFailed();

    /// Safe の setup の中（PSSafeSetup の delegatecall）から、Safe 自身が一度だけ呼ぶ。
    function register(address token, address user, address shopper, uint64 t1, uint64 t2) external {
        if (configs[msg.sender].token != address(0)) revert AlreadyRegistered();
        if (token == address(0) || user == address(0) || shopper == address(0) || t1 >= t2) revert InvalidConfig();
        configs[msg.sender] = Config({token: token, user: user, shopper: shopper, t1: t1, t2: t2});
        emit Registered(msg.sender, token, user, shopper, t1, t2);
    }

    function claimByShopper(address safe) external {
        Config memory c = _config(safe);
        if (msg.sender != c.shopper) revert Unauthorized();
        if (block.timestamp < c.t1) revert TooEarly();
        uint256 amount = _sweep(safe, c.token, c.shopper);
        emit ClaimedByShopper(safe, c.shopper, amount);
    }

    function refundToUser(address safe) external {
        Config memory c = _config(safe);
        if (msg.sender != c.user) revert Unauthorized();
        if (block.timestamp < c.t2) revert TooEarly();
        uint256 amount = _sweep(safe, c.token, c.user);
        emit RefundedToUser(safe, c.user, amount);
    }

    function config(address safe)
        external
        view
        returns (address token, address user, address shopper, uint64 t1, uint64 t2)
    {
        Config memory c = configs[safe];
        return (c.token, c.user, c.shopper, c.t1, c.t2);
    }

    function _config(address safe) private view returns (Config memory c) {
        c = configs[safe];
        if (c.token == address(0)) revert NotRegistered();
    }

    /// Safe の token 残高を全額 `to` へ送らせる。
    function _sweep(address safe, address token, address to) private returns (uint256 amount) {
        amount = IERC20(token).balanceOf(safe);
        bytes memory data = abi.encodeCall(IERC20.transfer, (to, amount));
        (bool ok, bytes memory ret) = ISafeModuleTarget(safe).execTransactionFromModuleReturnData(token, 0, data, 0);
        // transfer が false を返す token も失敗として扱う。
        if (!ok || (ret.length != 0 && !abi.decode(ret, (bool)))) revert TransferFailed();
    }
}
