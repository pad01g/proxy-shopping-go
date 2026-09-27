// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/// Safe v1.4.1 のうち、モジュールとセットアップが使う部分だけ。
interface ISafeModuleTarget {
    /// operation: 0 = Call, 1 = DelegateCall
    function execTransactionFromModuleReturnData(address to, uint256 value, bytes memory data, uint8 operation)
        external
        returns (bool success, bytes memory returnData);

    function enableModule(address module) external;
}
